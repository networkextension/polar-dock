package dock

import (
	"regexp"
	"testing"
)

// shortCodeRE matches the format the assets-module share-link generator
// will mint in P1c: 8–12 base62 characters. Pre-defining the test here
// in P0 lets P1c just drop in the generateAssetShareCode implementation
// and run the suite — no extra wire-up.
var shortCodeRE = regexp.MustCompile(`^[0-9A-Za-z]{8,12}$`)

func TestAssetShareCodeFormat(t *testing.T) {
	t.Skip("generateAssetShareCode lands in P1c; placeholder test")
	// for _, n := range []int{8, 10, 12} {
	// 	code := generateAssetShareCode(n)
	// 	if !shortCodeRE.MatchString(code) {
	// 		t.Errorf("code %q (n=%d) doesn't match %s", code, n, shortCodeRE)
	// 	}
	// }
}
