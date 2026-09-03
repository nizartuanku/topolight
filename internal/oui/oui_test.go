package oui

import "testing"

func TestVendor(t *testing.T) {
	cases := map[string]string{"00:1c:73:aa:bb:cc": "Arista Networks", "001c.73aa.bbcc": "Arista Networks", "3C5A37000000": "Samsung Electronics Co.,Ltd", "02:00:00:00:00:01": "locally administered", "zz": ""}
	for in, want := range cases {
		if got := Vendor(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
	if Size() < 40000 {
		t.Fatalf("table too small: %d", Size())
	}
}
