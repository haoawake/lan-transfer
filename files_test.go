package main

import (
	"strings"
	"testing"
)

func TestSanitizeName(t *testing.T) {
	cases := map[string]string{
		"photo.jpg":                  "photo.jpg",
		"../../etc/passwd":           "passwd",
		`C:\Users\a\Desktop\报告.docx`: "报告.docx",
		"a<b>c:d|e?f*g\".txt":        "a_b_c_d_e_f_g_.txt",
		"name. ":                     "name",
		"CON.txt":                    "_CON.txt",
		"nul":                        "_nul",
		"":                           "未命名文件",
		"..":                         "未命名文件",
		"tab\there\n.txt":            "tabhere.txt",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q，应该是 %q", in, got, want)
		}
	}
	long := strings.Repeat("很长的名字", 40) + ".mp4"
	got := sanitizeName(long)
	if len(got) > 200 || !strings.HasSuffix(got, ".mp4") {
		t.Errorf("长文件名应该截短并保留扩展名，得到 %d 字节 %q", len(got), got)
	}
}

func TestContentDisposition(t *testing.T) {
	got := contentDisposition("attachment", `报告 "最终版".pdf`)
	want := `attachment; filename="__ _____.pdf"; filename*=UTF-8''%E6%8A%A5%E5%91%8A%20%22%E6%9C%80%E7%BB%88%E7%89%88%22.pdf`
	if got != want {
		t.Errorf("得到 %s\n应该是 %s", got, want)
	}
}

func TestInlineOK(t *testing.T) {
	for _, c := range []struct {
		name string
		want bool
	}{
		{"a.jpg", true}, {"a.mp4", true}, {"a.pdf", true}, {"a.mp3", true},
		{"a.svg", false}, {"a.html", false}, {"a.exe", false}, {"a.txt", false},
	} {
		if got := inlineOK(mimeOf(c.name)); got != c.want {
			t.Errorf("inlineOK(%s) = %v", c.name, got)
		}
	}
}
