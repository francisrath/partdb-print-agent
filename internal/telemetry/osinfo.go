package telemetry

import (
	"bufio"
	"os"
	"strings"
)

// board is the single-board computer's model ("Raspberry Pi 2 Model B"), without its revision.
func board() string {
	b, err := os.ReadFile("/proc/device-tree/model")
	if err != nil {
		return ""
	}
	m, _, _ := strings.Cut(strings.TrimRight(string(b), "\x00\n"), " Rev ")
	if m = clip(m); !boardRe.MatchString(m) {
		return "" // only Raspberry Pi models are named; other boards' strings are free-form
	}
	return m
}

// linuxRelease is "<ID> <major VERSION_ID>" from os-release: "debian 13", "ubuntu 24".
func linuxRelease(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	kv := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if ok {
			kv[k] = strings.Trim(v, `"'`)
		}
	}
	id := kv["ID"]
	if !distros[id] {
		return "linux"
	}
	major, _, _ := strings.Cut(kv["VERSION_ID"], ".")
	if v := id + " " + major; major != "" && osVersionRe.MatchString(v) {
		return v
	}
	return id
}

// clip keeps only what Validate accepts (letters, digits, space . _ + -; 64 characters).
func clip(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 128 && (r == ' ' || r == '.' || r == '_' || r == '+' || r == '-' || ('0' <= r && r <= '9') || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z')) {
			return r
		}
		return -1
	}, s)
	s = strings.TrimLeft(s, " ._+-")
	if len(s) > 64 {
		s = s[:64]
	}
	return s
}
