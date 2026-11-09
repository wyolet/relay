package config

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// loadListenHosts reads RELAY_LISTEN_HOST and RELAY_CONTROL_LISTEN_HOST; the control one falls back to the shared one.
func loadListenHosts(cfg *Config) error {
	var err error
	if cfg.ListenHost, err = listenHost("RELAY_LISTEN_HOST"); err != nil {
		return err
	}
	if cfg.ControlListenHost, err = listenHost("RELAY_CONTROL_LISTEN_HOST"); err != nil {
		return err
	}
	if cfg.ControlListenHost == "" {
		cfg.ControlListenHost = cfg.ListenHost
	}
	return nil
}

// listenHost reads a bind address without a port. An IPv6 literal may come bracketed; the listener adds the brackets back when it joins the port.
func listenHost(key string) (string, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		v = v[1 : len(v)-1]
	}
	// A value that splits as host:port carries a port the *_PORT variable owns; joining a second one would fail at bind time instead of here.
	if _, _, err := net.SplitHostPort(v); err == nil || strings.ContainsAny(v, "[]/ ") {
		return "", fmt.Errorf("%s must be a host or IP address without a port, got %q", key, v)
	}
	return v, nil
}

// loadPprofAddr reads RELAY_PPROF_ADDR. Unlike the other listeners it takes host and port together: it has no default port to fall back on.
func loadPprofAddr(cfg *Config) error {
	v := strings.TrimSpace(os.Getenv("RELAY_PPROF_ADDR"))
	if v == "" {
		return nil
	}
	if _, _, err := net.SplitHostPort(v); err != nil {
		return fmt.Errorf("RELAY_PPROF_ADDR must be host:port (e.g. 127.0.0.1:6060), got %q", v)
	}
	cfg.PprofAddr = v
	for _, k := range []struct {
		key string
		dst *int
	}{{"RELAY_PPROF_MUTEX_FRACTION", &cfg.PprofMutexFraction}, {"RELAY_PPROF_BLOCK_RATE", &cfg.PprofBlockRate}} {
		s := strings.TrimSpace(os.Getenv(k.key))
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return fmt.Errorf("%s must be a non-negative integer, got %q", k.key, s)
		}
		*k.dst = n
	}
	return nil
}

// InferenceAddr is the address the inference listener binds.
func (c *Config) InferenceAddr() string { return net.JoinHostPort(c.ListenHost, c.Port) }

// ControlAddr is the address the control listener binds.
func (c *Config) ControlAddr() string { return net.JoinHostPort(c.ControlListenHost, c.ControlPort) }
