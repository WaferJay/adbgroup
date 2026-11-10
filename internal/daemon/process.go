package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func StateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "adbgroup", "daemon"), nil
}

func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("daemon directory %s must be a private directory (0700)", dir)
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("daemon directory must belong to the current user")
	}
	return nil
}

func acquireLock(ctx context.Context, path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func unlock(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }

func Unavailable(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

func WaitStopped(ctx context.Context, dir string) error {
	lock, err := acquireLock(ctx, filepath.Join(dir, "daemon.lock"))
	if err != nil {
		return err
	}
	unlock(lock)
	return nil
}

// Ensure serializes launchers and returns only after a versioned status handshake.
func Ensure(ctx context.Context, dir, executable string) error {
	if _, err := Call(ctx, dir, Request{Operation: "status"}); err == nil {
		return nil
	} else if !Unavailable(err) {
		return err
	}
	if err := prepareDir(dir); err != nil {
		return err
	}
	lock, err := acquireLock(ctx, filepath.Join(dir, "startup.lock"))
	if err != nil {
		return err
	}
	defer unlock(lock)
	if _, err := Call(ctx, dir, Request{Operation: "status"}); err == nil {
		return nil
	} else if !Unavailable(err) {
		return err
	}
	logPath := filepath.Join(dir, "daemon.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(executable, "daemon", "run")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	for {
		if _, err := Call(ctx, dir, Request{Operation: "status"}); err == nil {
			return nil
		} else if !Unavailable(err) {
			return err
		}
		select {
		case err := <-exited:
			return fmt.Errorf("daemon exited during startup (%v); see %s", err, logPath)
		case <-ctx.Done():
			return fmt.Errorf("daemon startup: %w; see %s", ctx.Err(), logPath)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Run holds a lifetime lock before removing any stale socket.
func Run(ctx context.Context, dir string) error {
	if err := prepareDir(dir); err != nil {
		return err
	}
	lockCtx, cancel := context.WithTimeout(ctx, time.Second)
	lock, err := acquireLock(lockCtx, filepath.Join(dir, "daemon.lock"))
	cancel()
	if err != nil {
		return fmt.Errorf("daemon already running or locked: %w", err)
	}
	defer unlock(lock)
	path := SocketPath(dir)
	if _, err := Call(ctx, dir, Request{Operation: "status"}); err == nil {
		return errors.New("daemon already running")
	} else if !Unavailable(err) {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer l.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return err
	}
	return Serve(ctx, l, NewManager())
}
