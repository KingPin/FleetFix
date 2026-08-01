package release

import (
	"slices"
	"testing"
)

// TestAmd64AssetNameIsFrozen is the single most consequential assertion in the
// package. Every fleetfix 1.6.0 install in the field looks for an asset by this
// exact name; a host that cannot find it reports "no update available" and stays
// on Python forever, with no error anyone would notice. The literal is written out
// here rather than composed from the constants so that changing either constant
// fails this test instead of quietly renaming the asset.
func TestAmd64AssetNameIsFrozen(t *testing.T) {
	got, err := AssetName("amd64")
	if err != nil {
		t.Fatalf("AssetName(amd64) errored: %v", err)
	}
	if got != "fleetfix-linux-x86_64" {
		t.Errorf("AssetName(amd64) = %q, want the frozen %q", got, "fleetfix-linux-x86_64")
	}
}

func TestAssetName(t *testing.T) {
	tests := []struct {
		goarch  string
		want    string
		wantErr bool
	}{
		{goarch: "amd64", want: "fleetfix-linux-x86_64"},
		{goarch: "arm64", want: "fleetfix-linux-aarch64"},
		// A GOARCH we do not publish must not get a plausible-looking name.
		{goarch: "386", wantErr: true},
		{goarch: "arm", wantErr: true},
		{goarch: "riscv64", wantErr: true},
		{goarch: "", wantErr: true},
		// GOARCH, not uname -m: passing the output name back in is a mistake.
		{goarch: "x86_64", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.goarch, func(t *testing.T) {
			got, err := AssetName(tt.goarch)
			if tt.wantErr {
				if err == nil {
					t.Errorf("AssetName(%q) = %q, want an error", tt.goarch, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("AssetName(%q) errored: %v", tt.goarch, err)
			}
			if got != tt.want {
				t.Errorf("AssetName(%q) = %q, want %q", tt.goarch, got, tt.want)
			}
		})
	}
}

func TestChecksumNameIsTheAssetPlusSuffix(t *testing.T) {
	for _, goarch := range Architectures() {
		asset, err := AssetName(goarch)
		if err != nil {
			t.Fatalf("AssetName(%q) errored: %v", goarch, err)
		}
		got, err := ChecksumName(goarch)
		if err != nil {
			t.Fatalf("ChecksumName(%q) errored: %v", goarch, err)
		}
		if got != asset+".sha256" {
			t.Errorf("ChecksumName(%q) = %q, want %q", goarch, got, asset+".sha256")
		}
	}
}

func TestChecksumNamePropagatesAnUnknownArch(t *testing.T) {
	if _, err := ChecksumName("mips"); err == nil {
		t.Error("ChecksumName(mips) returned no error")
	}
}

func TestEveryPublishedArchitectureHasAnAssetName(t *testing.T) {
	// Architectures() drives the release workflow's matrix. An entry with no asset
	// name would build a binary the upload step then cannot name.
	for _, goarch := range Architectures() {
		if _, err := AssetName(goarch); err != nil {
			t.Errorf("Architectures() lists %q but AssetName rejects it: %v", goarch, err)
		}
	}
}

func TestArchitecturesLeadsWithAmd64(t *testing.T) {
	// Ordering is not cosmetic: amd64 is the arch the existing fleet updates into,
	// so it is the one that must exist before a release is worth publishing.
	if got := Architectures(); len(got) == 0 || got[0] != "amd64" {
		t.Errorf("Architectures() = %v, want amd64 first", got)
	}
}

func TestAssetNamesAreUnique(t *testing.T) {
	var names []string
	for _, goarch := range Architectures() {
		name, err := AssetName(goarch)
		if err != nil {
			t.Fatalf("AssetName(%q) errored: %v", goarch, err)
		}
		if slices.Contains(names, name) {
			t.Fatalf("two architectures share the asset name %q; one would overwrite the other", name)
		}
		names = append(names, name)
	}
}
