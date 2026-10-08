package identity

import "testing"

func baseDescriptor() Descriptor {
	return Descriptor{
		TargetIQN:  "iqn.1991-05.com.microsoft:win-0e595h11jss-test-target",
		Serial:     "SN-0001",
		WWID:       "naa.6001405abcdef",
		BlockCount: 2097152,
		BlockSize:  512,
	}
}

func TestNamespaceIsStable(t *testing.T) {
	d := baseDescriptor()
	first := d.Namespace()
	for i := 0; i < 8; i++ {
		if got := d.Namespace(); got != first {
			t.Fatalf("namespace is not deterministic: %q vs %q", got, first)
		}
	}
	if len(first) != 64 {
		t.Fatalf("namespace length = %d, want a 64 character hex digest", len(first))
	}
}

// TestNamespaceChangesWithIdentity is the property the whole cache binding
// relies on: any change of the backing LUN must produce a different namespace.
func TestNamespaceChangesWithIdentity(t *testing.T) {
	base := baseDescriptor().Namespace()
	mutations := map[string]func(*Descriptor){
		"target iqn":  func(d *Descriptor) { d.TargetIQN = "iqn.other" },
		"serial":      func(d *Descriptor) { d.Serial = "SN-0002" },
		"wwid":        func(d *Descriptor) { d.WWID = "naa.other" },
		"block count": func(d *Descriptor) { d.BlockCount++ },
		"block size":  func(d *Descriptor) { d.BlockSize = 4096 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			d := baseDescriptor()
			mutate(&d)
			if got := d.Namespace(); got == base {
				t.Fatalf("changing %s did not change the namespace", name)
			}
		})
	}
}

// TestNamespaceFieldBoundaries checks that fields cannot be confused with each
// other: the length prefix keeps "ab"+"c" distinct from "a"+"bc".
func TestNamespaceFieldBoundaries(t *testing.T) {
	a := Descriptor{TargetIQN: "ab", Serial: "c"}.Namespace()
	b := Descriptor{TargetIQN: "a", Serial: "bc"}.Namespace()
	if a == b {
		t.Fatal("field boundaries are not delimited")
	}
}

func TestFull(t *testing.T) {
	if !baseDescriptor().Full() {
		t.Fatal("a descriptor with serial and wwid must be full")
	}
	if !(Descriptor{Serial: "x"}).Full() {
		t.Fatal("a serial alone is enough")
	}
	if !(Descriptor{WWID: " x "}).Full() {
		t.Fatal("a wwid alone is enough")
	}
	if (Descriptor{BlockCount: 1, BlockSize: 512}).Full() {
		t.Fatal("geometry alone must not count as identity")
	}
	if (Descriptor{Serial: "   "}).Full() {
		t.Fatal("blank fields must not count as identity")
	}
}

func TestEmptyDescriptor(t *testing.T) {
	if got := (Descriptor{}).Namespace(); got == "" || len(got) != 64 {
		t.Fatalf("empty descriptor namespace = %q", got)
	}
}
