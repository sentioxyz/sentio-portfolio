package portfolio

import "testing"

func TestSuilendObligationParentMatchesMainAndIsolatedObjects(t *testing.T) {
	// Independently fetched mainnet object owners, not computed expectations.
	for _, v := range []struct{ table, obligation, owner string }{
		{"0xcffebc0eb9d701d7843346dab87a043a60e28e595278db5764b87a4b3919a00a", "0x0821b92543ac31eabd7c038b7ef1a340436b5bb6cb74f95a5bfe432a2a25db7e", "0x046dc1d01094007bc0e32f1b87e0bdb8f24eda37f0b5f481a335a246734ae3d8"},
		{"0x07bb3a7f7a477dbff678e26682b66cd88fd66993febd707b1b9d3c46f4d20265", "0xd4347ed0573e253b1f07202530b6be64ef1f70f6aac1d6d8fbda7a1ff3769966", "0x00110ff329e6fa552e76c8d839eb3a3cb009e461ea9d45ae516dcfb984b84357"},
	} {
		got, err := suilendObligationParent(v.table, v.obligation)
		if err != nil || got != v.owner {
			t.Fatalf("derived owner %s, want %s: %v", got, v.owner, err)
		}
		key, _ := ParseSuiAddress(v.obligation)
		plain, err := suiCLMMFieldID(v.table, "0x2::object::ID", key[:])
		if err != nil || plain == got {
			t.Fatal("dynamic object wrapper tag was omitted")
		}
	}
}
