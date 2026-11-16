package iitc

import (
	"strings"
	"testing"
)

func TestParseUserscript(t *testing.T) {
	valid := "// ==UserScript==\n// @name  IITC plugin: Test\n// @version 1.2.3\n// ==/UserScript==\nfunction wrapper() {}\n"

	meta, err := ParseUserscript([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "IITC plugin: Test" || meta.Version != "1.2.3" {
		t.Fatalf("unexpected meta: %+v", meta)
	}

	bad := map[string]string{
		"empty":        "",
		"no header":    "function wrapper() {}",
		"unclosed":     "// ==UserScript==\n// @version 1.0.0\n",
		"no version":   "// ==UserScript==\n// @name x\n// ==/UserScript==\n",
		"invalid utf8": "// ==UserScript==\n// @version 1.0.0\n// ==/UserScript==\n\xff",
		"too large":    "// ==UserScript==\n// @version 1.0.0\n// ==/UserScript==\n" + strings.Repeat("x", MaxPluginBytes),
	}
	for name, code := range bad {
		if _, err := ParseUserscript([]byte(code)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSlugAndVersion(t *testing.T) {
	for _, s := range []string{"abc", "my-plugin", "a1-b2"} {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false", s)
		}
	}
	for _, s := range []string{"ab", "-abc", "abc-", "ABC", "a_b", strings.Repeat("a", 41)} {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true", s)
		}
	}
	for _, v := range []string{"1.0.0", "0.1.12", "1.0.0-beta.1", "2.3.4+build5"} {
		if !ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = false", v)
		}
	}
	for _, v := range []string{"1.0", "v1.0.0", "1.0.0 ", ""} {
		if ValidVersion(v) {
			t.Errorf("ValidVersion(%q) = true", v)
		}
	}
}
