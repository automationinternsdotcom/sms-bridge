// Package cookies reads the Google cookies libgm needs from a Firefox profile.
package cookies

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// Wanted maps a cookie name to the host it must come from.
var Wanted = map[string]string{
	"SID":              ".google.com",
	"HSID":             ".google.com",
	"SSID":             ".google.com",
	"APISID":           ".google.com",
	"SAPISID":          ".google.com",
	"__Secure-1PSIDTS": ".google.com",
	"OSID":             "messages.google.com",
}

var Required = []string{"SID", "HSID", "SSID", "APISID", "SAPISID", "OSID"}

// FromFirefoxProfile copies cookies.sqlite (plus its WAL) to a temp dir so it
// can be read while Firefox is running, then extracts the wanted cookies.
func FromFirefoxProfile(profileDir string) (map[string]string, error) {
	tmp, err := os.MkdirTemp("", "smsbridge-cookies-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src := filepath.Join(profileDir, "cookies.sqlite"+suffix)
		if err := copyFile(src, filepath.Join(tmp, "cookies.sqlite"+suffix)); err != nil && suffix == "" {
			return nil, fmt.Errorf("read %s: %w", src, err)
		}
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(tmp, "cookies.sqlite"))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name, value, host FROM moz_cookies WHERE host IN ('.google.com','messages.google.com','.messages.google.com') ORDER BY lastAccessed`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, value, host string
		if err := rows.Scan(&name, &value, &host); err != nil {
			return nil, err
		}
		want, ok := Wanted[name]
		if !ok {
			continue
		}
		if strings.TrimPrefix(host, ".") == strings.TrimPrefix(want, ".") {
			out[name] = value // later (more recently accessed) rows win
		}
	}
	var missing []string
	for _, r := range Required {
		if out[r] == "" {
			missing = append(missing, r)
		}
	}
	if len(missing) > 0 {
		return out, fmt.Errorf("missing cookies %v: sign in to messages.google.com in this Firefox profile first", missing)
	}
	return out, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
