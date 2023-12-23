// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package tailfs

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
	"tailscale.com/safesocket"
	"tailscale.com/tailfs/compositefs"
	"tailscale.com/tailfs/webdavfs"
	"tailscale.com/types/logger"
	"tailscale.com/util/pathutil"
)

var (
	disallowShareAs bool
)

// AllowShareAs indicates whether sharing files as a specific user is allowed
func AllowShareAs() bool {
	return !disallowShareAs && doAllowShareAs()
}

// Share represents a folder that's shared with remote Tailfs nodes.
type Share struct {
	// Name is how this share appears on remote nodes.
	Name string `json:"name"`
	// Path is the path to the directory on this machine that's being shared.
	Path string `json:"path"`
	// As is the UNIX or Windows username of the local account used for this
	// share. File read/write permissions are enforced based on this username.
	As string `json:"who"`
	// Readers is a list of Tailscale principals that are allowed to read this
	// share.
	Readers []string `json:"readers,omitempty"`
	// Writers is a list of Tailscale principals that are allowed to write to
	// this share.
	Writers []string `json:"writers,omitempty"`
}

// ForRemote is the TailFS filesystem exposed to remote nodes. It provides a
// unified WebDAV interface to local directories that have been shared.
type ForRemote interface {
	// SetFileServerAddr sets the address of the file server to which we
	// should proxy. This is used on platforms like Windows and MacOS
	// sandboxed where we can't spawn user-specific sub-processes and instead
	// rely on the UI application that's already running as an unprivileged
	// user to access the filesystem for us.
	SetFileServerAddr(addr string)

	// SetShares sets the complete set of shares exposed by this node. If
	// AllowShareAs() is true, we will use one subprocess per user to access
	// the filesystem (see userServer). Otherwise, we will use the file server
	// configured via SetFileServerAddr.
	SetShares(shares map[string]*Share)

	// ServeHTTP behaves like the similar method from http.Handler but also
	// accepts a Permissions map that captures the permissions of the connecting
	// node.
	ServeHTTP(permissions Permissions, w http.ResponseWriter, r *http.Request)

	// Close() stops serving the WebDAV content
	Close() error
}

func NewFileSystemForRemote(logf logger.Logf) ForRemote {
	fs := &fileSystemForRemote{
		logf:        logf,
		lockSystem:  webdav.NewMemLS(),
		fileSystems: make(map[string]webdav.FileSystem),
		userServers: make(map[string]*userServer),
	}
	return fs
}

type fileSystemForRemote struct {
	logf           logger.Logf
	lockSystem     webdav.LockSystem
	fileServerAddr string
	shares         map[string]*Share
	fileSystems    map[string]webdav.FileSystem
	userServers    map[string]*userServer
	mx             sync.RWMutex
}

func (s *fileSystemForRemote) SetFileServerAddr(addr string) {
	s.mx.Lock()
	s.fileServerAddr = addr
	s.mx.Unlock()
}

func (s *fileSystemForRemote) SetShares(shares map[string]*Share) {
	userServers := make(map[string]*userServer)
	if AllowShareAs() {
		// set up per-user server
		for _, share := range shares {
			p, found := userServers[share.As]
			if !found {
				p = &userServer{
					logf: s.logf,
				}
				userServers[share.As] = p
			}
			p.shares = append(p.shares, share)
		}
		for _, p := range userServers {
			go p.runLoop()
		}
	}

	fileSystems := make(map[string]webdav.FileSystem, len(shares))
	for _, share := range shares {
		fileSystems[share.Name] = webdavfs.New(&webdavfs.Opts{
			Logf: s.logf,
			URL:  fmt.Sprintf("http://%v/%v", share.Name, share.Name),
			Transport: &http.Transport{
				Dial: func(_, shareAddr string) (net.Conn, error) {
					shareName, _, err := net.SplitHostPort(shareAddr)
					if err != nil {
						return nil, fmt.Errorf("unable to parse share address %v: %w", shareAddr, err)
					}

					s.mx.RLock()
					share, shareFound := s.shares[shareName]
					userServers := s.userServers
					fileServerAddr := s.fileServerAddr
					s.mx.RUnlock()

					if !shareFound {
						return nil, fmt.Errorf("unknown share %v", shareName)
					}

					var addr string
					if !AllowShareAs() {
						addr = fileServerAddr
					} else {
						userServer, found := userServers[share.As]
						if found {
							userServer.mx.RLock()
							addr = userServer.addr
							userServer.mx.RUnlock()
						}
					}

					if addr == "" {
						return nil, fmt.Errorf("unable to determine address for share %v", shareName)
					}

					_, err = netip.ParseAddrPort(addr)
					if err == nil {
						// this is a regular network address, dial normally
						return net.Dial("tcp", addr)
					}
					// assume this is a safesocket address
					return safesocket.Connect(addr)
				},
			},
			StatRoot: true,
		})
	}

	s.mx.Lock()
	s.shares = shares
	oldFileSystems := s.fileSystems
	oldUserServers := s.userServers
	s.fileSystems = fileSystems
	s.userServers = userServers
	s.mx.Unlock()

	s.stopUserServers(oldUserServers)
	s.closeFileSystems(oldFileSystems)
}

func (s *fileSystemForRemote) ServeHTTP(permissions Permissions, w http.ResponseWriter, r *http.Request) {
	isWrite := writeMethods[r.Method]
	if isWrite {
		share := pathutil.Split(r.URL.Path)[0]
		switch permissions.For(share) {
		case PermissionNone:
			// If we have no permissions to this share, treat it as not found
			// to avoid leaking any information about the share's existence.
			http.Error(w, "not found", http.StatusNotFound)
			return
		case PermissionReadOnly:
			http.Error(w, "permission denied", http.StatusForbidden)
			return
		}
	}

	s.mx.RLock()
	fileSystems := s.fileSystems
	s.mx.RUnlock()

	children := make(map[string]webdav.FileSystem, len(fileSystems))
	// filter out shares to which the connecting principal has no access
	for name, fs := range fileSystems {
		if permissions.For(name) == PermissionNone {
			continue
		}

		children[name] = fs
	}

	cfs := compositefs.New(
		&compositefs.Opts{
			Logf:         s.logf,
			StatChildren: true,
		})
	cfs.SetChildren(children)
	h := webdav.Handler{
		FileSystem: cfs,
		LockSystem: s.lockSystem,
	}
	h.ServeHTTP(w, r)
}

func (s *fileSystemForRemote) stopUserServers(userServers map[string]*userServer) {
	for _, server := range userServers {
		if err := server.Close(); err != nil {
			s.logf("error closing tailfs user server: %v", err)
		}
	}
}

func (s *fileSystemForRemote) closeFileSystems(fileSystems map[string]webdav.FileSystem) {
	for _, fs := range fileSystems {
		closer, ok := fs.(interface{ Close() error })
		if ok {
			if err := closer.Close(); err != nil {
				s.logf("error closing tailfs filesystem: %v", err)
			}
		}
	}
}

func (s *fileSystemForRemote) Close() error {
	s.mx.Lock()
	userServers := s.userServers
	fileSystems := s.fileSystems
	s.mx.Unlock()

	s.stopUserServers(userServers)
	s.closeFileSystems(fileSystems)
	return nil
}

// userServer runs tailscaled serve-tailfs to serve webdav content for the
// given Shares. All Shares are assumed to have the same Share.As, and the
// content is served as that Share.As user.
type userServer struct {
	logf   logger.Logf
	shares []*Share
	closed bool
	cmd    *exec.Cmd
	addr   string
	mx     sync.RWMutex
}

func (s *userServer) Close() error {
	s.mx.Lock()
	cmd := s.cmd
	s.closed = true
	s.mx.Unlock()
	if cmd != nil && cmd.Process != nil {
		return cmd.Process.Kill()
	}
	// not running, that's okay
	return nil
}

func (s *userServer) runLoop() {
	executable, err := os.Executable()
	if err != nil {
		s.logf("can't find executable: %v", err)
		return
	}
	for {
		s.mx.RLock()
		closed := s.closed
		s.mx.RUnlock()
		if closed {
			return
		}

		err := s.run(executable)
		s.logf("user server % v stopped with error %v, will start again", executable, err)
		// TODO(oxtoacart): maybe be smarter about backing off here
		time.Sleep(1 * time.Second)
	}
}

// Run runs the executable (tailscaled). This function only works on UNIX systems,
// but those are the only ones on which we use userServers anyway.
func (s *userServer) run(executable string) error {
	// set up the command
	args := []string{"serve-tailfs"}
	for _, s := range s.shares {
		args = append(args, s.Name, s.Path)
	}
	allArgs := []string{"-u", s.shares[0].As, executable}
	allArgs = append(allArgs, args...)
	cmd := exec.Command("sudo", allArgs...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	defer stdout.Close()
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	defer stderr.Close()

	err = cmd.Start()
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	s.mx.Lock()
	s.cmd = cmd
	s.mx.Unlock()

	// read address
	stdoutScanner := bufio.NewScanner(stdout)
	stdoutScanner.Scan()
	if stdoutScanner.Err() != nil {
		return fmt.Errorf("read addr: %w", stdoutScanner.Err())
	}
	addr := stdoutScanner.Text()
	// send the rest of stdout and stderr to logger to avoid blocking
	go func() {
		for stdoutScanner.Scan() {
			s.logf("tailscaled serve-tailfs stdout: %v", stdoutScanner.Text())
		}
	}()
	stderrScanner := bufio.NewScanner(stderr)
	go func() {
		for stderrScanner.Scan() {
			s.logf("tailscaled serve-tailfs stderr: %v", stderrScanner.Text())
		}
	}()
	s.mx.Lock()
	s.addr = strings.TrimSpace(addr)
	s.mx.Unlock()
	return cmd.Wait()
}

var writeMethods = map[string]bool{
	"PUT":       true,
	"POST":      true,
	"COPY":      true,
	"LOCK":      true,
	"UNLOCK":    true,
	"MKCOL":     true,
	"MOVE":      true,
	"PROPPATCH": true,
}
