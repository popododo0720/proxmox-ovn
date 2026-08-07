package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

func listenUnix(path string) (net.Listener, error) {
	return listenUnixForGroup(path, "")
}

func listenBrowserUnix(path string, peerUsers []string, socketGroup string) (net.Listener, error) {
	listener, err := listenUnixForGroup(path, socketGroup)
	if err != nil {
		return nil, err
	}
	allowedUIDs := make(map[uint32]struct{}, len(peerUsers))
	for _, peerUser := range peerUsers {
		uid, lookupErr := lookupUserID(peerUser)
		if lookupErr == nil {
			allowedUIDs[uint32(uid)] = struct{}{}
			continue
		}
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, lookupErr
	}
	if len(allowedUIDs) == 0 {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, errors.New("browser Unix listener requires at least one peer user")
	}
	return &peerUIDListener{Listener: listener, allowedUIDs: allowedUIDs}, nil
}

func listenRootUnix(path string) (net.Listener, error) {
	listener, err := listenUnixForGroup(path, "")
	if err != nil {
		return nil, err
	}
	return &peerUIDListener{
		Listener:    listener,
		allowedUIDs: map[uint32]struct{}{0: {}},
	}, nil
}

func listenUnixForGroup(path, groupName string) (net.Listener, error) {
	directory := filepath.Dir(path)
	if err := prepareSocketDirectory(directory, groupName); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket path %q", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale Unix socket: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect Unix socket path: %w", err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on Unix socket: %w", err)
	}
	cleanup := func(problem error) (net.Listener, error) {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, problem
	}
	if groupName != "" {
		gid, lookupErr := lookupGroupID(groupName)
		if lookupErr != nil {
			return cleanup(lookupErr)
		}
		if err := os.Chown(path, -1, gid); err != nil {
			return cleanup(fmt.Errorf("set Unix socket group %q: %w", groupName, err))
		}
	}
	if err := os.Chmod(path, 0o660); err != nil {
		return cleanup(fmt.Errorf("set Unix socket permissions: %w", err))
	}
	return listener, nil
}

func prepareSocketDirectory(path, groupName string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Unix socket directory %q must be a real directory", path)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o750); err != nil {
			return fmt.Errorf("create Unix socket directory: %w", err)
		}
	} else {
		return fmt.Errorf("inspect Unix socket directory: %w", err)
	}
	if groupName == "" {
		return nil
	}
	gid, err := lookupGroupID(groupName)
	if err != nil {
		return err
	}
	if err := os.Chown(path, -1, gid); err != nil {
		return fmt.Errorf("set Unix socket directory group %q: %w", groupName, err)
	}
	if err := os.Chmod(path, 0o2750); err != nil {
		return fmt.Errorf("set Unix socket directory permissions: %w", err)
	}
	return nil
}

func lookupUserID(name string) (int, error) {
	account, err := user.Lookup(name)
	if err != nil {
		return 0, fmt.Errorf("look up Unix user %q: %w", name, err)
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid < 0 {
		return 0, fmt.Errorf("Unix user %q has invalid UID %q", name, account.Uid)
	}
	return uid, nil
}

func lookupGroupID(name string) (int, error) {
	group, err := user.LookupGroup(name)
	if err != nil {
		return 0, fmt.Errorf("look up Unix group %q: %w", name, err)
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil || gid < 0 {
		return 0, fmt.Errorf("Unix group %q has invalid GID %q", name, group.Gid)
	}
	return gid, nil
}

type peerUIDListener struct {
	net.Listener
	allowedUIDs map[uint32]struct{}
}

func (listener *peerUIDListener) Accept() (net.Conn, error) {
	for {
		connection, err := listener.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, credentialErr := unixPeerUID(connection)
		if credentialErr == nil && listener.allowsUID(uid) {
			return connection, nil
		}
		_ = connection.Close()
	}
}

func (listener *peerUIDListener) allowsUID(uid uint32) bool {
	_, allowed := listener.allowedUIDs[uid]
	return allowed
}

func unixPeerUID(connection net.Conn) (uint32, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, errors.New("gateway peer is not a Unix connection")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("access gateway peer socket: %w", err)
	}
	var credential *unix.Ucred
	var socketErr error
	if err := raw.Control(func(fileDescriptor uintptr) {
		credential, socketErr = unix.GetsockoptUcred(int(fileDescriptor), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, fmt.Errorf("inspect gateway peer socket: %w", err)
	}
	if socketErr != nil {
		return 0, fmt.Errorf("read gateway peer credentials: %w", socketErr)
	}
	return credential.Uid, nil
}
