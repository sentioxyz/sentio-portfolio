package portfolio

import (
	"os"
	"testing"
)

// Corpora for the account-scoped indexers. Every address below currently holds at least one
// open ticket or request in its protocol's index, and most hold several, so the comparison
// exercises multi-row pagination rather than a single lucky row.
//
// The DeBank project identifiers are asserted by the fetch helper: an identifier the account
// does not carry fails loudly rather than silently reconciling an empty position.

func TestEtherfiDeBankAPIReconciliation(t *testing.T) {
	if os.Getenv("PORTFOLIO_DEBANK_API_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEBANK_API_LIVE_TEST=1 to compare with DeBank Pro API")
	}
	adapter := newEtherfiAdapter(liveSentioIndexerConfig(t, "PORTFOLIO_ETHERFI_INDEXER"))
	runDeBankAPIReconciliationRefreshed(t, "etherfi", adapter, Ethereum, []string{
		"0x036663d2d46170a0a6f76f1c55a08902e3a3d3ca",
		"0x04cba06e5bba3c1b64e8483a1edd5a06b90520ab",
		"0x1d1c8da29cfe967061e015e7277ec5cb1ae916fd",
		"0x269737605564906b99051e904e1a41da5c7ba045",
		"0x2ae0e97bd7bb4fc5b442181e7879f8dd8d56d163",
		"0x32b7fd1da51b71825a1618ca37545d6ec0a54226",
		"0x3ebf18295a8dff98350f2d5f1d4d1a1580667f76",
		"0x3f91c148cfac16cc430d021d9cb191f3e2722917",
		"0x452f439bd2dde893ae35fdee508c7e06680faf25",
		"0x468c4e33c7e70528da901254bcb88714cb7be452",
		"0x623806c35bcf2b1ca8ceb2724c050b2707ea882b",
		"0xe869473fadedddfe2fbc1a939856eda0de3dbb1b",
		"0x6bde0df33494d2903ac445e06c4701dabd3266ea",
		"0x93b993c386dbd1665bce76b13bb7ad9a45d6e214",
		"0x93f436575f8104ba0f7871ca9d89544b898d4607",
		"0x9eccadf3860098e9a8c455c5dfad6e4126cbc25e",
		"0xae5be9ddb562d34b9e54f5ebbafd2ef787967c52",
		"0xc21dc05b60adf722302b7865d01ce74511e33006",
		"0xd5d730a0cff08294473e54d0567f132005517050",
		"0xd7dedcf8ffddceed9ad20a41a5f88dc1268b3ad4",
	})
}

// Membership coverage had never been reconciled: the withdrawal-request corpus above happens to
// hold no membership NFTs, so 20/20 passed while the ERC-1155 surface was entirely untested.
// Every address here holds at least one membership token with non-zero valueOf, drawn from a
// full replay of the membership contract, and the last one holds ten so the comparison
// exercises multi-row pagination through the ERC-1155 side of the index.
func TestEtherfiMembershipDeBankAPIReconciliation(t *testing.T) {
	if os.Getenv("PORTFOLIO_DEBANK_API_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEBANK_API_LIVE_TEST=1 to compare with DeBank Pro API")
	}
	adapter := newEtherfiAdapter(liveSentioIndexerConfig(t, "PORTFOLIO_ETHERFI_INDEXER"))
	runDeBankAPIReconciliationRefreshed(t, "etherfi", adapter, Ethereum, []string{
		"0x0016cd225fe61a366dcb3dd702dfa6ab8c35059b",
		"0x05290e2507a710b4f1e99669eab1c5c939b37561",
		"0x0aecb90c68d456049fc837aea69bece78d69a801",
		"0x139cb586de20faf734ed6ede51ef026bb6813918",
		"0x1a1ef606b4fbce3ab05aef5610002ee95b8bde8a",
		"0x213706f372cc0983d45cb00fee1ef77e519e0939",
		"0x26d125498742b99b521b838f31df87b22947dd5e",
		"0x2e1c8f80c1937edd277c984bb32d5ebdc3a7786b",
		"0x3387c3ced164f7b9711d6fb322493d19d10ea673",
		"0x3a0aa58d203e33c239ed4be37b86683d9e699cfc",
		"0x3f9ed6abd69eb17908793f018f7be1b1daf31dde",
		"0x44f0e650a559fd905228047ca90c3d7d696c609e",
		"0x4b2f599b332fdbca7c43991aae288fd261d33c4f",
		"0x5155dfb16d5b4442f7fb56978b7ac8cc30fd2ae7",
		"0x560075ba1990dded3b8cb49f1955e607999c942d",
		"0x5d57f8c508b1151bed75d6070d118445901083eb",
		"0x638c23e630fdc227010f3270d9d77ccaafdaeb1d",
		"0x6822e0c109f82070dbd7e05b7761d36372176ee6",
		"0x6d8b3dbc99cea0736451cc01bdf598269952f525",
		"0x731bfadaf268f6a388558fad07cfcc5ec9e9323f",
	})
}

func TestFraxEtherDeBankAPIReconciliation(t *testing.T) {
	if os.Getenv("PORTFOLIO_DEBANK_API_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEBANK_API_LIVE_TEST=1 to compare with DeBank Pro API")
	}
	adapter := newFraxEtherAdapter(liveSentioIndexerConfig(t, "PORTFOLIO_FRAX_ETHER_INDEXER"))
	runDeBankAPIReconciliationRefreshed(t, "frax", adapter, Ethereum, []string{
		"0x0463e60c7ce10e57911ab7bd1667eaa21de3e79b",
		"0x07cb1b3a52faf636a52822d918b07d30b0914d76",
		"0x082515bbe6533aae58a1159651944d311e6d1bda",
		"0x0b02f6c9fc6d49e537cfa5b3283758f9c8ba4046",
		"0x0bba917af1a0470454f6534c79584c52e2ea1a44",
		"0x0da91b336e11a8a629727374ec243bd5084c5425",
		"0x0e098632c947f7158d9dd3349a824c5e0402b8df",
		"0x0ee7e120fa705c1ea08ab78fdaf8a96d935bb242",
		"0x0f8eae0a899a18051635f91d6e5c2521e7e161fe",
		"0x1b996bd16a3f1dad0e094847d6ce555648bd6a29",
		"0x2adc93ae56cc48628aab52c277b5419925504f54",
		"0x66c68194ecaddafcc72e4f5fdd3fa9f7058a6194",
		"0x8c4a52f67b91dfc271c7375d9abf55f676ff5d71",
		"0xa26014c216be64182bcffaeefb3f4a7afb17e934",
		"0xb7cfeba2dd84fdc943c50125efaa7335d3785fe6",
		"0xbe988fc9f6f8ad1ebb3a58b6c25bd6be9d1f56fe",
		"0xc53ba20e3d584f317f9abfa8cde8659f680507ef",
		"0xdc866e97539ec0eff42c6ddfed6bd6a573e26d91",
		"0xe33fae378ab2eb2b8e82cc57a8f503b02a6f0498",
		"0xf1c7e19a34cad5c49a79dcdea2406668fb1dcb8b",
	})
}

func TestRenzoDeBankAPIReconciliation(t *testing.T) {
	if os.Getenv("PORTFOLIO_DEBANK_API_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEBANK_API_LIVE_TEST=1 to compare with DeBank Pro API")
	}
	adapter := newRenzoAdapter(liveSentioIndexerConfig(t, "PORTFOLIO_RENZO_INDEXER"))
	runDeBankAPIReconciliationRefreshed(t, "renzo", adapter, Ethereum, []string{
		"0x0085cc4d9ce979174819da928d0b07979c9a9846",
		"0x0330681819826086a816e750849894ad5895ac9a",
		"0x047981f832f71ba5e4d2e06fbaf21b83c6898e33",
		"0x04e19eed5fd3f2402f0f7e95425c498f4c62d02e",
		"0x05901e97b7b2575f4e807566a4db382fdf248017",
		"0x09e3ab1ce8126bc411d7132cb4673409a60023cd",
		"0x0b1a89664970ebeb16d6a1a039017049eea45a20",
		"0x0d423e7877c579a4bde7c70c4bbaa49fbc69720e",
		"0x0e67d51dd9b34bfe45a96686b983c20864d05f62",
		"0x10c9293927a4c05ee148e7019f4a12b7a177ff80",
		"0x12df219156b01f1ae144848deb5b0e530f3d1ebd",
		"0x23e68a3ba783e4f287843e06d7c4dcbe9c94fb17",
		"0x2731018d041ed1c230a20f3bee72f0c6decae3aa",
		"0x3334c2dfbd6584ba98506bca76140be42aebd62c",
		"0x66faa5ded68cc83a71bc172fb03c17a6bcb32f7a",
		"0x86e9def0c1b531fd868368bc8cd24154d8e4e806",
		"0x92adb9900cea82e65a71cb08e670780601679625",
		"0x96b539339b3149dcc2337fe434e0460ff43de327",
		"0xad0f3f4bec42cda68d2cd31301b3c3de3b0f50e2",
		"0xb0cc4ed0cc3c2231581eaf6fea6f422928a23f4f",
	})
}

func TestAsterDeBankAPIReconciliation(t *testing.T) {
	if os.Getenv("PORTFOLIO_DEBANK_API_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEBANK_API_LIVE_TEST=1 to compare with DeBank Pro API")
	}
	adapter := newAsterAdapter(liveSentioIndexerConfig(t, "PORTFOLIO_ASTER_INDEXER"))
	// DeBank still files Aster under its former name, Astherus.
	runDeBankAPIReconciliationRefreshed(t, "bsc_astherus", adapter, BSC, []string{
		"0x007692a074ebf040917f83ea8bd846ecfd52d5a0",
		"0x00a23895e63221b4394b5d9d62cfb1569fa8d279",
		"0x01335066f99ba51203556fb82285094f455366f2",
		"0x021acdae4dc0ebd5cafc374e91dbaabf03c4f2e4",
		"0x0321e89a7b9484f47e8ccd4daf97ee8066783c04",
		"0x054599d02df8150ac13b0a3ec5e4b507b24b36c0",
		"0x0671fe13e970966b0f7ca41e50d38f621834198c",
		"0x06b3800398e84947ce37721ead29331dcdc11bc6",
		"0x093c27602d299555a905ee61fc5f1a52cda058ed",
		"0x0b26fd8a595b2baa5eb265c8ba7d042889ea80f5",
		"0x0c768b5839f87cf859898ef7017ee5013dcd3768",
		"0x0dcf7535c57554d0e26e43936bbefc37f23e37fb",
		"0x0e0b6b12717d029047bc039460016782ce458e0e",
		"0x46883c68b162d28e20854e3a3349b4624ba3e87b",
		"0x4a72f7261b09345cc5c49839aac935593a2e5d03",
		"0x7a634619eed1ba77fbe8c70a734fc1b8f1f1a1a8",
		"0xabd8cee5a93265fc7d1f9e45f0169294d01b8802",
		"0xcf72e66f87b63f51d0a419628f4d8768ae21bab1",
		"0xeb69833005d9aa0bd571f27fe15c076ac8ab7668",
		"0xf6c8e540dc80f223092119dab8e19223530d6605",
	})
}
