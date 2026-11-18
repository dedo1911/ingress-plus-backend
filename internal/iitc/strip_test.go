package iitc

import (
	"strings"
	"testing"
)

const stripSample = `// ==UserScript==
// @name IITC plugin: Sample
// @version 1.2.3
// ==/UserScript==

/*! Sample plugin, GPL-3.0 */
// a line comment
function wrapper(plugin_info) {
  /* block comment */
  var url = 'https://intel.ingress.com/'; // trailing
  var re = /\/\/not-a-comment/;
  var tpl = ` + "`/* kept */ ${url}`" + `;
  window.SAMPLE = [url, re, tpl, "// also kept", 'ünicode'];
}
`

func TestStripComments(t *testing.T) {
	out, err := StripComments([]byte(stripSample))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	if !strings.HasPrefix(got, "// ==UserScript==\n// @name IITC plugin: Sample\n// @version 1.2.3\n// ==/UserScript==\n") {
		t.Errorf("metadata block not kept at the top:\n%s", got)
	}
	if _, err := ParseUserscript(out); err != nil {
		t.Errorf("stripped code no longer parses as a userscript: %v", err)
	}
	for _, gone := range []string{"a line comment", "block comment", "trailing"} {
		if strings.Contains(got, gone) {
			t.Errorf("comment %q survived:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"GPL-3.0", "https://intel.ingress.com/", `\/\/not-a-comment`, "/* kept */", "// also kept", "ünicode"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q went missing:\n%s", kept, got)
		}
	}
	if len(out) >= len(stripSample) {
		t.Errorf("stripped code isn't smaller: %d >= %d", len(out), len(stripSample))
	}
}

func TestStripCommentsSyntaxError(t *testing.T) {
	if _, err := StripComments([]byte("// ==UserScript==\n// ==/UserScript==\nfunction (")); err == nil {
		t.Fatal("expected an error for invalid JavaScript")
	}
}
