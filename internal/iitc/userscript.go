package iitc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	// MaxPluginBytes is the largest plugin file the store accepts. IITC's own
	// biggest plugins are a few hundred KB; anything near this is probably
	// bundling something it shouldn't.
	MaxPluginBytes = 1 << 20

	maxHeaderLines = 200
)

var (
	slugPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)
	versionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+([-.+][0-9A-Za-z.-]+)?$`)
	metaLine       = regexp.MustCompile(`^//\s*@(\S+)\s*(.*)$`)
)

// ValidSlug reports whether slug can be used as a plugin's permanent id.
func ValidSlug(slug string) bool { return slugPattern.MatchString(slug) }

// ValidVersion reports whether version looks like semver.
func ValidVersion(version string) bool { return versionPattern.MatchString(version) }

// UserscriptMeta holds the header fields the store looks at.
type UserscriptMeta struct {
	Name    string
	Version string
}

// ParseUserscript checks that code is a userscript with a metadata block and
// returns the fields of interest. IITC plugins are userscripts, and the
// header is the one place an author states the version they meant to ship,
// so the store holds the upload to it.
func ParseUserscript(code []byte) (UserscriptMeta, error) {
	var meta UserscriptMeta

	if len(code) == 0 {
		return meta, errors.New("the file is empty")
	}
	if len(code) > MaxPluginBytes {
		return meta, fmt.Errorf("the file is larger than %d KB", MaxPluginBytes>>10)
	}
	if !utf8.Valid(code) {
		return meta, errors.New("the file is not valid UTF-8 text")
	}

	scanner := bufio.NewScanner(bytes.NewReader(code))
	scanner.Buffer(make([]byte, 0, 64*1024), MaxPluginBytes)

	inHeader, closed := false, false
	lines := 0
	for scanner.Scan() && !closed {
		line := strings.TrimSpace(scanner.Text())
		if !inHeader {
			if line == "// ==UserScript==" {
				inHeader = true
			}
			continue
		}
		lines++
		if lines > maxHeaderLines {
			break
		}
		if line == "// ==/UserScript==" {
			closed = true
			break
		}
		m := metaLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch m[1] {
		case "name":
			meta.Name = strings.TrimSpace(m[2])
		case "version":
			meta.Version = strings.TrimSpace(m[2])
		}
	}

	if !inHeader || !closed {
		return meta, errors.New("missing the // ==UserScript== ... // ==/UserScript== header")
	}
	if meta.Version == "" {
		return meta, errors.New("the userscript header has no @version")
	}
	return meta, nil
}
