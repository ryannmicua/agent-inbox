package inbox

import "testing"

func TestBuildVersionPrefersReleaseStamp(t *testing.T) {
	saved := version
	t.Cleanup(func() { version = saved })

	version = ""
	if got := BuildVersion(); got == "" {
		t.Fatal("BuildVersion() without a release stamp is empty")
	}

	version = "v1.2.3"
	if got := BuildVersion(); got != "v1.2.3" {
		t.Fatalf("BuildVersion() = %q, want the release stamp v1.2.3", got)
	}
}
