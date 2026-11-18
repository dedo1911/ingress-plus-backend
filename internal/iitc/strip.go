package iitc

import (
	"bytes"
	"errors"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

// StripComments returns code without its comments, to make the copy served to
// the sideloader smaller.
//
// A regular expression can't do this safely ("//" shows up in strings, URLs,
// regex and template literals), so the code goes through esbuild, which
// parses it and prints it back. Kept on purpose:
//   - the ==UserScript== metadata block, put back at the top as it was;
//   - legal comments (/*! ... */, //!, @license, @preserve), because most
//     IITC plugins are GPL and the notice has to travel with the code.
//
// esbuild reprints the code (indentation, quote style), but it doesn't
// minify, rename or reorder anything. The one kind of plugin this can break
// reads its own source through Function.prototype.toString and expects the
// comments to still be there, which is rare enough that the caller just
// serves the original when this fails.
func StripComments(code []byte) ([]byte, error) {
	result := api.Transform(string(code), api.TransformOptions{
		Loader:        api.LoaderJS,
		Charset:       api.CharsetUTF8,
		LegalComments: api.LegalCommentsInline,
		LogLevel:      api.LogLevelSilent,
		Sourcefile:    "plugin.user.js",
	})
	if len(result.Errors) > 0 {
		return nil, errors.New(result.Errors[0].Text)
	}

	header := userscriptHeader(code)
	out := make([]byte, 0, len(header)+len(result.Code))
	out = append(out, header...)
	out = append(out, result.Code...)
	return out, nil
}

// userscriptHeader returns the metadata block, from the ==UserScript== line
// through ==/UserScript== and its newline, or nil if there isn't a closed one.
func userscriptHeader(code []byte) []byte {
	var header []byte
	inHeader := false
	for rest := code; len(rest) > 0; {
		line := rest
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i+1], rest[i+1:]
		} else {
			rest = nil
		}
		trimmed := strings.TrimSpace(string(line))
		if !inHeader {
			if trimmed == "// ==UserScript==" {
				inHeader = true
				header = append(header, line...)
			}
			continue
		}
		header = append(header, line...)
		if trimmed == "// ==/UserScript==" {
			if header[len(header)-1] != '\n' {
				header = append(header, '\n')
			}
			return header
		}
		if len(header) > MaxPluginBytes {
			break
		}
	}
	return nil
}

// strippedCache holds StripComments output per version. Versions can't be
// changed once uploaded (see guardVersionUpdate), so an entry never goes
// stale; the cap only bounds memory.
var strippedCache = struct {
	sync.Mutex
	entries map[string][]byte
}{entries: map[string][]byte{}}

const strippedCacheMax = 64

func cachedStrip(key string, code []byte) ([]byte, error) {
	strippedCache.Lock()
	hit, ok := strippedCache.entries[key]
	strippedCache.Unlock()
	if ok {
		return hit, nil
	}

	stripped, err := StripComments(code)
	if err != nil {
		return nil, err
	}

	strippedCache.Lock()
	if len(strippedCache.entries) >= strippedCacheMax {
		clear(strippedCache.entries)
	}
	strippedCache.entries[key] = stripped
	strippedCache.Unlock()
	return stripped, nil
}
