package media

import "testing"

func TestSignRequest(t *testing.T) {
	// the worked example in Cloudinary's own documentation
	got := signRequest(map[string]string{"timestamp": "1315060510"}, "abcd")
	if want := "a21ad0f63beb4de2e5575204b79ab90bffb02c10"; got != want {
		t.Errorf("Cloudinary's example: got %s, want %s", got, want)
	}

	base := signRequest(map[string]string{"timestamp": "1", "public_id": "a"}, "s")
	same := map[string]map[string]string{
		"excluded parameters don't count": {"timestamp": "1", "public_id": "a", "api_key": "k", "file": "f", "cloud_name": "c", "resource_type": "raw"},
		"empty values don't count":        {"timestamp": "1", "public_id": "a", "type": ""},
	}
	for name, params := range same {
		if signRequest(params, "s") != base {
			t.Errorf("%s: the signature changed", name)
		}
	}
	if signRequest(map[string]string{"timestamp": "1", "public_id": "b"}, "s") == base {
		t.Error("a changed value kept the same signature")
	}
	if signRequest(map[string]string{"timestamp": "1", "public_id": "a"}, "other") == base {
		t.Error("a different secret kept the same signature")
	}
	// parameters are sorted by name, whatever order they're given in
	if got, want := signRequest(map[string]string{"z": "1", "a": "2"}, "s"), signRequest(map[string]string{"a": "2", "z": "1"}, "s"); got != want {
		t.Error("order changed the signature")
	}
}
