package textutil

import (
	"testing"
	"unicode/utf8"
)

func TestUTF8Safe(t *testing.T) {
	s := "你好世界abc"
	for n := 0; n <= len(s)+1; n++ {
		if h := Head(s, n); !utf8.ValidString(h) || len(h) > n {
			t.Fatalf("Head(%d)=%q", n, h)
		}
		if tl := Tail(s, n); !utf8.ValidString(tl) || len(tl) > n {
			t.Fatalf("Tail(%d)=%q", n, tl)
		}
	}
	if Truncate("你好", 4, "…") != "你…" {
		t.Fatal(Truncate("你好", 4, "…"))
	}
}
