// Package oui maps the first three bytes of a MAC address to the vendor that
// registered them. The table is the IEEE MA-L registry (via the oui-data
// package, BSD-2-Clause), compacted to "oui name" lines and gzipped; it is
// inflated on first use only.
package oui

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/hex"
	"strings"
	"sync"
)

//go:embed oui.txt.gz
var table []byte

var (
	once sync.Once
	// three registries: MA-L (24-bit), MA-M (28-bit) and MA-S (36-bit) prefixes,
	// keyed by the hex prefix string; longest match wins
	tab24, tab28, tab36 map[string]string
)

func load() {
	tab24, tab28, tab36 = map[string]string{}, map[string]string{}, map[string]string{}
	zr, err := gzip.NewReader(bytes.NewReader(table))
	if err != nil {
		return
	}
	sc := bufio.NewScanner(zr)
	for sc.Scan() {
		line := sc.Text()
		sp := strings.IndexByte(line, ' ')
		if sp < 6 {
			continue
		}
		key, name := line[:sp], strings.TrimSpace(line[sp+1:])
		switch len(key) {
		case 6:
			tab24[key] = name
		case 7:
			tab28[key] = name
		case 9:
			tab36[key] = name
		}
	}
}

// Vendor returns the registrant for a MAC ("aa:bb:cc:dd:ee:ff", "aabb.ccdd.eeff"
// or raw hex) or "" when unknown. Locally administered addresses (bit 2 of the
// first byte) are reported as "locally administered" — randomised Wi-Fi MACs
// from phones and laptops land here.
func Vendor(mac string) string {
	once.Do(load)
	h := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			return r
		case r >= 'A' && r <= 'F':
			return r + 32
		}
		return -1
	}, mac)
	if len(h) < 6 {
		return ""
	}
	if len(h) >= 9 {
		if v, ok := tab36[h[:9]]; ok {
			return v
		}
	}
	if len(h) >= 7 {
		if v, ok := tab28[h[:7]]; ok {
			return v
		}
	}
	if v, ok := tab24[h[:6]]; ok {
		return v
	}
	if b, err := hex.DecodeString(h[:2]); err == nil && b[0]&0x02 != 0 {
		return "locally administered"
	}
	return ""
}

// Size reports the number of entries (for Admin → System).
func Size() int {
	once.Do(load)
	return len(tab24) + len(tab28) + len(tab36)
}
