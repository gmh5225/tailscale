// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package tailfs

import (
	"log"
	"net"
	"net/http"

	"golang.org/x/net/webdav"
	"tailscale.com/connlistener"
	"tailscale.com/tailfs/compositefs"
	"tailscale.com/tailfs/webdavfs"
	"tailscale.com/types/logger"
)

// ForLocal is the TailFS filesystem exposed to local clients. It provides a
// unified WebDAV interface to remote TailFS shares on other nodes.
type ForLocal interface {
	// SetRemotes sets the complete set of remotes on the given tailnet domain
	// using a map of name -> url. If transport is specified, that transport
	// will be used to connect to these remotes.
	SetRemotes(domain string, namesToURLS map[string]string, transport http.RoundTripper)

	// HandleConn handles connections from local WebDAV clients
	HandleConn(conn net.Conn, remoteAddr net.Addr) error

	// Close() stops serving the WebDAV content
	Close() error
}

// NewFileSystemForLocal starts serving a filesystem for local clients.
// Inbound connections must be handed to HandleConn.
func NewFileSystemForLocal(logf logger.Logf) ForLocal {
	fs := &fileSystemForLocal{logf: logf}
	fs.serveAt()
	return fs
}

type fileSystemForLocal struct {
	logf     logger.Logf
	cfs      compositefs.CompositeFileSystem
	listener connlistener.Listener
}

func (s *fileSystemForLocal) serveAt() {
	s.cfs = compositefs.New(&compositefs.Opts{Logf: s.logf})
	s.listener = connlistener.New()

	hs := &http.Server{Handler: &webdav.Handler{
		FileSystem: s.cfs,
		LockSystem: webdav.NewMemLS(),
	}}
	go func() {
		err := hs.Serve(s.listener)
		if err != nil {
			// TODO(oxtoacart): should we panic or something different here?
			log.Printf("serve: %v", err)
		}
	}()
}

func (s *fileSystemForLocal) HandleConn(conn net.Conn, remoteAddr net.Addr) error {
	return s.listener.HandleConn(conn, remoteAddr)
}

func (s *fileSystemForLocal) SetRemotes(domain string, namesToURLS map[string]string, transport http.RoundTripper) {
	remotes := make(map[string]webdav.FileSystem, len(namesToURLS))
	for name, url := range namesToURLS {
		opts := &webdavfs.Opts{
			URL:          url,
			Transport:    transport,
			StatCacheTTL: statCacheTTL,
			Logf:         s.logf,
		}
		remotes[name] = webdavfs.New(opts)
	}

	domainChild, found := s.cfs.GetChild(domain)
	if !found {
		domainChild = compositefs.New(&compositefs.Opts{Logf: s.logf})
		s.cfs.SetChildren(map[string]webdav.FileSystem{domain: domainChild})
	}
	domainChild.(compositefs.CompositeFileSystem).SetChildren(remotes)
}

func (s *fileSystemForLocal) Close() error {
	s.cfs.Close()
	return s.listener.Close()
}
