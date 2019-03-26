// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util

import (
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/coreos/ignition/config/v3_0/types"
	"github.com/coreos/ignition/internal/log"
	"github.com/coreos/ignition/internal/resource"
	"github.com/coreos/ignition/internal/util"
)

const (
	DefaultDirectoryPermissions os.FileMode = 0755
	DefaultFilePermissions      os.FileMode = 0644
)

type FetchOp struct {
	Hash         hash.Hash
	Url          url.URL
	FetchOptions resource.FetchOptions
	Append       bool
	Node         types.Node
}

// newHashedReader returns a new ReadCloser that also writes to the provided hash.
func newHashedReader(reader io.ReadCloser, hasher hash.Hash) io.ReadCloser {
	return struct {
		io.Reader
		io.Closer
	}{
		Reader: io.TeeReader(reader, hasher),
		Closer: reader,
	}
}

func newFetchOp(l *log.Logger, node types.Node, contents types.FileContents) (FetchOp, error) {
	var expectedSum []byte

	uri, err := url.Parse(*contents.Source)
	if err != nil {
		return FetchOp{}, err
	}

	hasher, err := util.GetHasher(contents.Verification)
	if err != nil {
		l.Crit("Error verifying file %q: %v", node.Path, err)
		return FetchOp{}, err
	}

	if hasher != nil {
		// explicitly ignoring the error here because the config should already
		// be validated by this point
		_, expectedSumString, _ := util.HashParts(contents.Verification)
		expectedSum, err = hex.DecodeString(expectedSumString)
		if err != nil {
			l.Crit("Error parsing verification string %q: %v", expectedSumString, err)
			return FetchOp{}, err
		}
	}
	compression := ""
	if contents.Compression != nil {
		compression = *contents.Compression
	}

	return FetchOp{
		Hash: hasher,
		Node: node,
		Url:  *uri,
		FetchOptions: resource.FetchOptions{
			Hash:        hasher,
			Compression: compression,
			ExpectedSum: expectedSum,
		},
	}, nil
}

// PrepareFetches converts a given logger, http client, and types.File into a
// FetchOp. This includes operations such as parsing the source URL, generating
// a hasher, and performing user/group name lookups. If an error is encountered,
// the issue will be logged and nil will be returned.
func (u Util) PrepareFetches(l *log.Logger, f types.File) ([]FetchOp, error) {
	ops := []FetchOp{}

	if f.Contents.Source != nil {
		if base, err := newFetchOp(l, f.Node, f.Contents); err != nil {
			return nil, err
		} else {
			ops = append(ops, base)
		}
	}

	for _, appendee := range f.Append {
		if op, err := newFetchOp(l, f.Node, appendee); err != nil {
			return nil, err
		} else {
			op.Append = true
			ops = append(ops, op)
		}
	}

	return ops, nil
}

func (u Util) WriteLink(s types.Link) error {
	path := s.Path

	if err := MkdirForFile(path); err != nil {
		return fmt.Errorf("Could not create leading directories: %v", err)
	}

	if s.Hard != nil && *s.Hard {
		targetPath, err := u.JoinPath(s.Target)
		if err != nil {
			return err
		}
		return os.Link(targetPath, path)
	}

	if err := os.Symlink(s.Target, path); err != nil {
		return fmt.Errorf("Could not create symlink: %v", err)
	}

	uid, gid, err := u.ResolveNodeUidAndGid(s.Node, 0, 0)
	if err != nil {
		return err
	}

	if err := os.Lchown(path, uid, gid); err != nil {
		return err
	}

	return nil
}

func (u Util) SetPermissions(f types.File) error {
	if f.Mode != nil {
		mode := os.FileMode(*f.Mode)
		if err := os.Chmod(f.Path, mode); err != nil {
			return err
		}
	}

	defaultUid, defaultGid, _ := getFileOwnerAndMode(f.Path)
	uid, gid, err := u.ResolveNodeUidAndGid(f.Node, defaultUid, defaultGid)
	if err != nil {
		return err
	}
	return os.Chown(f.Path, uid, gid)
}

// PerformFetch performs a fetch operation generated by PrepareFetch, retrieving
// the file and writing it to disk. Any encountered errors are returned.
func (u Util) PerformFetch(f FetchOp) error {
	path := f.Node.Path

	if err := MkdirForFile(path); err != nil {
		return err
	}

	// Create a temporary file in the same directory to ensure it's on the same filesystem
	tmp, err := ioutil.TempFile(filepath.Dir(path), "tmp")
	if err != nil {
		return err
	}
	defer tmp.Close()

	// ioutil.TempFile defaults to 0600
	if err := tmp.Chmod(DefaultFilePermissions); err != nil {
		return err
	}

	// sometimes the following line will fail (the file might be renamed),
	// but that's ok (we wanted to keep the file in that case).
	defer os.Remove(tmp.Name())

	err = u.Fetcher.Fetch(f.Url, tmp, f.FetchOptions)
	if err != nil {
		u.Crit("Error fetching file %q: %v", path, err)
		return err
	}

	if f.Append {
		// Make sure that we're appending to a file
		finfo, err := os.Lstat(path)
		switch {
		case os.IsNotExist(err):
			// No problem, we'll create it.
			break
		case err != nil:
			return err
		default:
			if !finfo.Mode().IsRegular() {
				return fmt.Errorf("can only append to files: %q", path)
			}
		}

		// Open with the default permissions, we'll chown/chmod it later
		targetFile, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, DefaultFilePermissions)
		if err != nil {
			return err
		}
		defer targetFile.Close()

		if _, err = tmp.Seek(0, os.SEEK_SET); err != nil {
			return err
		}
		if _, err = io.Copy(targetFile, tmp); err != nil {
			return err
		}
	} else {
		if err = os.Rename(tmp.Name(), path); err != nil {
			return err
		}
	}

	return nil
}

// MkdirForFile helper creates the directory components of path.
func MkdirForFile(path string) error {
	return os.MkdirAll(filepath.Dir(path), DefaultDirectoryPermissions)
}

// PathExists returns true if a node exists within DestDir, false otherwise. Any
// error other than ENOENT is treated as fatal.
func (u Util) PathExists(path string) (bool, error) {
	path, err := u.JoinPath(path)
	if err != nil {
		return false, err
	}

	_, err = os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

// getFileOwner will return the uid and gid for the file at a given path. If the
// file doesn't exist, or some other error is encountered when running stat on
// the path, 0, 0, and 0 will be returned.
func getFileOwnerAndMode(path string) (int, int, os.FileMode) {
	finfo, err := os.Stat(path)
	if err != nil {
		return 0, 0, 0
	}
	return int(finfo.Sys().(*syscall.Stat_t).Uid), int(finfo.Sys().(*syscall.Stat_t).Gid), finfo.Mode()
}

// ResolveNodeUidAndGid attempts to convert a types.Node into a concrete uid and
// gid. If the node has the User.ID field set, that's used for the uid. If the
// node has the User.Name field set, a username -> uid lookup is performed. If
// neither are set, it returns the passed in defaultUid. The logic is identical
// for gids with equivalent fields.
func (u Util) ResolveNodeUidAndGid(n types.Node, defaultUid, defaultGid int) (int, int, error) {
	var err error
	uid, gid := defaultUid, defaultGid

	if n.User.ID != nil {
		uid = *n.User.ID
	} else if n.User.Name != nil && *n.User.Name != "" {
		uid, err = u.getUserID(*n.User.Name)
		if err != nil {
			return 0, 0, err
		}
	}

	if n.Group.ID != nil {
		gid = *n.Group.ID
	} else if n.Group.Name != nil && *n.Group.Name != "" {
		gid, err = u.getGroupID(*n.Group.Name)
		if err != nil {
			return 0, 0, err
		}
	}
	return uid, gid, nil
}

func (u Util) getUserID(name string) (int, error) {
	usr, err := u.userLookup(name)
	if err != nil {
		return 0, fmt.Errorf("No such user %q: %v", name, err)
	}
	uid, err := strconv.ParseInt(usr.Uid, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("Couldn't parse uid %q: %v", usr.Uid, err)
	}
	return int(uid), nil
}

func (u Util) getGroupID(name string) (int, error) {
	g, err := u.groupLookup(name)
	if err != nil {
		return 0, fmt.Errorf("No such group %q: %v", name, err)
	}
	gid, err := strconv.ParseInt(g.Gid, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("Couldn't parse gid %q: %v", g.Gid, err)
	}
	return int(gid), nil
}

func (u Util) DeletePathOnOverwrite(n types.Node) error {
	if n.Overwrite == nil || !*n.Overwrite {
		return nil
	}
	return os.RemoveAll(n.Path)
}
