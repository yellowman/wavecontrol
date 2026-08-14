package firmware

import "testing"

func TestInferRebootPlatformFromFlavor(t *testing.T) {
	cases := map[string]string{
		"GMC.ipq5018":    "wave",
		"MGMP":           "wave",
		"MW":             "wave",
		"GP":             "wave",
		"AFLTUROCKET":    "ltu",
		"AFLTU":          "ltu",
		"AF5XHD":         "ltu",
		"XC":             "airmax",
		"2XC":            "airmax",
		"WA":             "airmax",
		"XM":             "airmax",
		"XW":             "airmax",
		"AF11":           "airfiber",
		"":               "",
		"unknown.flavor": "",
	}
	for flavor, want := range cases {
		if got := inferRebootPlatformFromFlavor(flavor); got != want {
			t.Fatalf("inferRebootPlatformFromFlavor(%q)=%q, want %q", flavor, got, want)
		}
	}
}
