package manifest

import (
	"strings"
	"testing"
)

const good = `{
 "schema":"oaudio.manifest.v1","name":"m","tag":"t","task":"stt","engine":"e",
 "upstream":{"model":"org/m"},
 "source":{"type":"huggingface","repo":"org/m","revision":"0123456789abcdef0123456789abcdef01234567",
   "files":[{"path":"a.bin","size":3,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]},
 "license":{"spdx":"MIT","commercial_use":true},
 "audio":{"sample_rate":16000}}`

func TestParse(t *testing.T) {
	m, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if m.Ref() != "m:t" || m.TotalSize() != 3 {
		t.Errorf("ref %q size %d", m.Ref(), m.TotalSize())
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string][2]string{
		"branch instead of commit": {`"revision":"0123456789abcdef0123456789abcdef01234567"`, `"revision":"main"`},
		"path traversal":           {`"path":"a.bin"`, `"path":"../../a.bin"`},
		"absolute path":            {`"path":"a.bin"`, `"path":"/etc/passwd"`},
		"missing hash":             {`,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`, ``},
		"no upstream credit":       {`"upstream":{"model":"org/m"}`, `"upstream":{"model":""}`},
		"no license":               {`"spdx":"MIT"`, `"spdx":""`},
		"unknown field":            {`"engine":"e"`, `"engine":"e","engnie":"typo"`},
		"bad task":                 {`"task":"stt"`, `"task":"asr"`},
	}
	for name, c := range cases {
		bad := strings.Replace(good, c[0], c[1], 1)
		if bad == good {
			t.Fatalf("%s: replacement did not apply", name)
		}
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSplitRef(t *testing.T) {
	if n, tag, err := SplitRef("whisper"); err != nil || n != "whisper" || tag != "latest" {
		t.Errorf("got %q %q %v", n, tag, err)
	}
	if n, tag, err := SplitRef("whisper:base.en-q5_1"); err != nil || n != "whisper" || tag != "base.en-q5_1" {
		t.Errorf("got %q %q %v", n, tag, err)
	}
	for _, bad := range []string{"", "a/b", "../x", "A B", "x:../y"} {
		if _, _, err := SplitRef(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
