// Package creds stores the machine credential obtained at registration.
// File: $HELM_HOME/credentials (JSON, mode 0600). The token never goes on a command line.
package creds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Credentials struct {
	Server    string `json:"server"`
	MachineID string `json:"machine_id"`
	Token     string `json:"token"`
}

func Path(helmHome string) string { return filepath.Join(helmHome, "credentials") }

func Load(helmHome string) (Credentials, error) {
	var c Credentials
	p := Path(helmHome)
	st, err := os.Stat(p)
	if err != nil {
		return c, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return c, fmt.Errorf("%s is readable by others (mode %o); run: chmod 600 %s", p, st.Mode().Perm(), p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", p, err)
	}
	if c.Server == "" || c.Token == "" {
		return c, errors.New("credentials file is incomplete; run `helmd register` again")
	}
	return c, nil
}

func Save(helmHome string, c Credentials) error {
	if err := os.MkdirAll(helmHome, 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := Path(helmHome) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, Path(helmHome))
}

// Register exchanges a one-time registration code for a machine token.
func Register(ctx context.Context, server, code, name string) (Credentials, error) {
	server = strings.TrimRight(server, "/")
	body, _ := json.Marshal(map[string]string{"code": code, "name": name})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server+"/daemon/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Credentials{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return Credentials{}, fmt.Errorf("registration failed: %d %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var out struct {
		MachineID string `json:"machineId"`
		Token     string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Credentials{}, err
	}
	return Credentials{Server: server, MachineID: out.MachineID, Token: out.Token}, nil
}
