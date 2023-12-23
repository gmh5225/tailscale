// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package compositefs

import (
	"context"
	"io/fs"
	"os"

	"golang.org/x/net/webdav"
	"tailscale.com/tailfs/shared"
	"tailscale.com/util/pathutil"
)

func (cfs *compositeFileSystem) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	if pathutil.IsRoot(name) {
		// the root directory contains one directory for each child
		di, err := cfs.Stat(ctx, name)
		if err != nil {
			return nil, err
		}
		return &shared.DirFile{
			Info: di,
			LoadChildren: func() ([]fs.FileInfo, error) {
				cfs.childrenMu.Lock()
				children := cfs.children
				cfs.childrenMu.Unlock()

				childInfos := make([]fs.FileInfo, 0, len(cfs.children))
				for _, c := range children {
					var childInfo fs.FileInfo
					if cfs.statChildren {
						fi, err := c.fs.Stat(ctx, "/")
						if err != nil {
							return nil, err
						}
						childInfo = &shared.StaticFileInfo{
							Named:    c.name, // we use the full name
							Sized:    fi.Size(),
							Moded:    fi.Mode(),
							ModTimed: fi.ModTime(),
							Dir:      fi.IsDir(),
						}
					} else {
						// always use now() as the modified time to bust caches
						childInfo = shared.ReadOnlyDirInfo(c.name, cfs.now())
					}
					childInfos = append(childInfos, childInfo)
				}
				return childInfos, nil
			},
		}, nil
	}

	path, onChild, child, err := cfs.pathToChild(name)
	if err != nil {
		return nil, err
	}

	if !onChild {
		// this is the child itself, ask it to open its root
		return child.fs.OpenFile(ctx, "/", flag, perm)
	}

	return child.fs.OpenFile(ctx, path, flag, perm)
}
