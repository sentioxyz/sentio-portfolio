package portfolio

// DeBank reconciliation corpora for the 2026-08-20 all-protocols sweep, locking in the
// protocols reconciled that day (internal notes: portfolio-all-protocols-debank-reconciliation,
// 2026-08-20). Corpora were drawn from recent on-chain activity, screened against DeBank for a
// live non-dust position, and admitted only when the sweep (or the in-process rerun with the
// fixes from that session) reconciled them cleanly.

import (
	"os"
	"testing"
)

// sweepAdapterByID resolves a registered adapter, so these corpora exercise exactly what the
// engine ships rather than a hand-built twin.
func sweepAdapterByID(t *testing.T, protocolID string) Adapter {
	t.Helper()
	for _, adapter := range NewEngine(nil, nil).adapters {
		if adapter.Info().ID == protocolID {
			return adapter
		}
	}
	t.Fatalf("adapter %s is not registered", protocolID)
	return nil
}

func TestAaveV3SweepDeBankAPIReconciliation(t *testing.T) {
	runDeBankAPIReconciliationRefreshed(t, "aave3", sweepAdapterByID(t, "aave-v3"), Ethereum, []string{
		"0xc58837214b25dcb5fe8750e0dffd3632ccad190c",
		"0x90deceec188094f6f6c1ef446d843f70abfc92cb",
		"0xfee79d7f033f94b318ecfd3bd9f999f5618ffdab",
		"0xd48738c363a720a4f70f687435a24675367aed09",
		"0xbec69dfce4c1fa8b7843fee1ca85788d84a86b06",
		"0x8daf9a6c784dc623179577804b0753b436288bde",
		"0x0bfc9d54fc184518a81162f8fb99c2eaca081202",
		"0xe28660e51e08f988aef28e46421305e83eb479e8",
		"0x10584baf934f41c2a18de9acd5066437592c3e3e",
		"0x4c806574b7d4259a2cd6885be4bdcf7ae75c4041",
		"0xd411d428a63cf4c7029bc53f0e0f56c4933fdbb7",
		"0x601024f09e63abdfdfe73b6f2c798431a3611062",
		"0x04455c2b38b3c4caffed24efe2cecb9fbeabb6b8",
		"0x57b68c4ea221ee8da6eb14ebdfccee5177567771",
		"0x891dee950c0225388e088daea90996ad6fd70d64",
		"0xd5eff808e5e4cd2717cef292e32a1d8b2d6a780a",
		"0xff003fbe8b8d5e7f271a9cb9f2780003daed2aa8",
		"0x98e9e70353a4e36a563ee9ae89887d8be18d70fa",
		"0xb5b636f1f6a449b432854a36b9eb70c67b0fc453",
		"0x525a9e9ebfa3f56df839515483b75a2a026baa27",
	})
}

func TestAaveV4SweepDeBankAPIReconciliation(t *testing.T) {
	runDeBankAPIReconciliationRefreshed(t, "aave4", sweepAdapterByID(t, "aave-v4"), Ethereum, []string{
		"0x910bfe37282fb543562f25657a77e94ebe8c8998",
		"0xe23742b0caf200cb894cf00f38ee830257208283",
		"0xb4d8620b65ba4a16ce57ee4f65793f1093dfae39",
		"0xd7ad196009fbe5c4210db626719af5439d43e5b9",
		"0x32febc1dd51a629d164f772fd8ddc6723163fa96",
		"0x97d0aca4142e89f968fe1ac0d206752e73f3b2b0",
		"0xc09ef136254132c3081b7eb4ae77b6030f054c32",
		"0xb4f8c33e5fc12dd00e42b02ee584abcf06405124",
		"0x2e3b0198493d874a203e93e37fd7a1137224fab9",
		"0x894a133b6e91fe68e06e4fdffcf5d267e4499eec",
		"0x14db15581e5404c38ec5770c82714f07e71a0135",
		"0x7a92a91d004aea97379f14f3a211938457a7938a",
		"0xf11380773c12c44f56148b472c791bc468f769db",
		"0xe0cb74aec5bd452e6958e756345708afcd11d56f",
		"0x37f1e7dcad2206e58678cdf43941b6f9b631c91a",
		"0x362627a677a0a82261bfc3cc6c2df30f032c29c0",
		"0x658eca63e200b2fa9c524687369de474e7ef1822",
		"0xd136bd82e6fe321653c7b05ed6e074bb4e9b9489",
		"0xa87cb885ee667466c9551deb4fbca0f0598b2fb4",
		"0x9ed5c8e16336240c701e122ce4fa6450fb8f0f7f",
	})
}

func TestCompoundV3SweepDeBankAPIReconciliation(t *testing.T) {
	runDeBankAPIReconciliationRefreshed(t, "compound3", sweepAdapterByID(t, "compound-v3"), Ethereum, []string{
		"0xe9a2c20c778fe3c4e9c9388542001ae9711b8261",
		"0x9a8ba5c16fc8223df664b9a212baab8ba57b3074",
		"0x6d1b730aa76b613ac1e52f73f0cb70c42b50331c",
		"0x75844eafb99841d72f3fe6dcfa522da01626559b",
		"0xb819706e897eacf235cdb5048962bd65873202c4",
		"0x0802e9bf4deaacf597b4df2454972a190ca7811a",
		"0x943b69b4679909ab4645c7cb43c2bbe90500c75b",
		"0x3b88fee469bf6c134278cbaef20ab3633cbc4999",
		"0x2d6fd0fd73f43e272669501fc80acaed223d9520",
		"0xb9e62cb9b4ce8ec13c886fae67369da417ee2714",
		"0xa7b1b0acb52afd5e12a24338ea462908401298cb",
		"0x73be9526629e0c615ce22603e9efd2f3ef9b523a",
		"0x2fc476342699b9d62973784fc620bd6925cac63d",
		"0xaa1544f4fb36f3c208e1f28a800bddb8c3e574d6",
		"0x5a84894c64ef6a31b41871086ecd27e94f80decd",
		"0xfdd5570df76d3569df7ca831ca8131b4bfe979b1",
		"0xc0ab3a27335a17f3fd60d7f0a184726198dd1aa3",
		"0x892b3c926cf8a84f94834ffefbb703e2d0db9c7c",
		"0xa5e808a06713e8af8384eae3a792ba7ec5ff6eed",
		"0x2c4e5183edf9c89c45a1ef6376f1f6434ad1fe45",
	})
}

func TestMakerDAOSweepDeBankAPIReconciliation(t *testing.T) {
	runDeBankAPIReconciliationRefreshed(t, "makerdao", sweepAdapterByID(t, "makerdao"), Ethereum, []string{
		"0x27acd464e08a70a8422ae06798b11181a0853ed3",
		"0xb5871a7180f9d0a89719338115418d5a9bb5de0f",
		"0xfb73cd2b333cdc6cf00a2374d88268410e3a44f8",
		"0xb5be1dc4d0493c9d1a47beb61d2d50facc1b2972",
		"0x7e2ff31ca5d88165bc05b3dce6c77ba6939572e4",
		"0x73781b91d63c39cb785689fa4b61a3967dfc3abc",
		"0x559117defbd5391b434c0bbf3cfcca8e6a442af2",
		"0xc63fc49b0b48457b2a5f89a0cb3bec872e634c6a",
		"0x0fa3643bec9228b2e10a91b52b6dec4ebbb4bdd3",
		"0x785389d2b4f50e7a22ca5c0d298a948695feb767",
		"0x441c4dba87561cfa2b0473e820d35ecb501bcbcf",
		"0xcb63d28e8e12d6a69402dea97be86b5e6c488cd4",
		"0x297835a86defcca77fca6e05f114b75117999b7d",
		"0x966ed92dbba2c50b2c047b6139098384eca2fafb",
		"0x3fc7194abb1230488399e3df4ccb519cd8774a81",
		"0x42c5cebf21a267316b6acd139e8c9f9385221cb7",
		"0x4ef6900770bc0042bbfc408c90142fd2f6d63103",
		"0x13ad7b14bda771b75efb6bb6330d1caa7b4c4d03",
		"0xd848f54280f8fe8661b796e3bb8d8922c87af452",
		"0x87de588d54fdc06a16c54da1f19e611a002a2e15",
	})
}

// Accounts whose DeBank surface includes the ENA LP-farming lock ("Farming")
// are excluded: the adapter deliberately indexes sUSDe/sENA staking and cooldowns only.
func TestEthenaSweepDeBankAPIReconciliation(t *testing.T) {
	runDeBankAPIReconciliationRefreshed(t, "ethena", sweepAdapterByID(t, "ethena"), Ethereum, []string{
		"0x000000000004444c5dc75cb358380d2e3de08a90",
		"0x47ab5f9d8c9c7d002a92320f23a696d348c56a7f",
		"0xbf98480425a29197e5d99d003017f63a1e595d02",
		"0xa36ecca8b7624d224f01cd6649c8afad3da12c3d",
		"0x727bb9155011a95ffff4e39f700a768836dd1214",
		"0xd1418b61f385b52a46f8c2a18db47c91a0d4fab1",
		"0xde7259893af7cdbc9fd806c6ba61d22d581d5667",
		"0x3942f7b55094250644cffda7160226caa349a38e",
		"0x03d7a747c406654da6b1b4342afddbbd9cd89e36",
		"0x3cef1afc0e8324b57293a6e7ce663781bbefbb79",
		"0x5c2ab69eb2bf12a2f4572d178687bd4660512972",
		"0x2289f64e11a3c6a3e23d5f0c705bb0bb7661278a",
		"0x42715ba91deda3c692b9f540cee2fbb4dae78bbb",
		"0x2df87810fcf9b8e6a42adc5923bc2ee0ca0467ca",
		"0xd7583e3cf08bbcab66f1242195227bbf9f865fda",
		"0xfd52e9a7f2547268c2329ec2984cf0d719b55b4d",
		"0x56a284acbd7318380fc8a380cd5695bb14b5a0ce",
		"0x5bfcd9fe67a606a6b6756db3db647ea166e91aa0",
		"0x9008d19f58aabd9ed0d60971565aa8510560ab41",
		"0xc06ebbefd94032b85424d51906e2a335efae264b",
	})
}

// Accounts with positions outside the adapter's four surfaces (stETH, wstETH,
// the withdrawal queue, earnETH) are excluded — DeBank's lido project also counts
// stMATIC, GGV and other side vaults.
func TestLidoSweepDeBankAPIReconciliation(t *testing.T) {
	runDeBankAPIReconciliationRefreshed(t, "lido", sweepAdapterByID(t, "lido"), Ethereum, []string{
		"0xcb8804d2df7173da7634a6e303149429590105da",
		"0x00000000008d5f1200332af8a6813cb8377b5bfd",
		"0x0b8a49d816cc709b6eadb09498030ae3416b66dc",
		"0xd271821d24825e5fb3afecf445d49ec79026cd2e",
		"0x2cc319a747b0c151e40c1a4639367dbee10af351",
		"0xf48dfb71271dc3a3d076fb819fc9db26942a3557",
		"0x04bd55e4f99212a9311b6de9592baccbb855e034",
		"0x323a39e777e203135cc87832efcfccad2813aa80",
		"0xccd40ac3e23a593b3aea28b3c74a07d3b46c0182",
		"0x9e6e86e0aa043d456eb58ff1c197c0e75f5fd759",
		"0xf018ec28e5ef2d23c64c0f59dd1b4f2539bcebe0",
		"0x15cbeb60385208b96a020ec51930aeac87c27ee9",
		"0x5265f8f84d8e043f2439bbe40de0db3208b226e7",
		"0xdc24316b9ae028f1497c275eb9192a3ea0f67022",
		"0xc035a7cf15375ce2706766804551791ad035e0c2",
		"0xdb57fdf5fd24a9d0e1ea94552eb2c7bdcb28fa27",
		"0x00000000000014aa86c5d3c41765bb24e11bd701",
		"0xf9c6251bb769bd04c9d7eb1599f4e1c5cb3208d3",
		"0x4a4aaa0155237881fbd5c34bfae16e985a7b068d",
		"0x8cd52ee292313c4d851e71a7064f096504ab3ee9",
	})
}

// Accounts holding sOHM v1.0 or legacy (pre-Mono) Cooler loans are excluded:
// DeBank counts those surfaces and the adapter deliberately does not.
func TestOlympusSweepDeBankAPIReconciliation(t *testing.T) {
	runDeBankAPIReconciliationRefreshed(t, "olympusdao", sweepAdapterByID(t, "olympus"), Ethereum, []string{
		"0x4cb20f4776dd84132849f3bd914ba1912379397f",
		"0x36831704d74b410742e311cc12817d4eb00427bb",
		"0x78f286786856932f2096efa2579b4b207b3f1738",
		"0xa3752af0d28549a04b2bb90c9fb5533d380c8ffa",
		"0x8a9b68f1f5e442f0388dcb059dfd909d9c350c25",
		"0xcf7e21b96a7dae8e1663b5a266fd812cbe973e70",
		"0x6220e96dce2351336d5a6e598b6d185662d36e2c",
		"0xe6c5dc8508939b1c417f83b7fed3149ef22cf3ff",
		"0x099a2c055fdd02fb432e59e04bc4b7cf73e1dd69",
		"0x406c50a27fe5e8cd928fbdf7f9c2356a0d6b7248",
		"0xafca7d645ac2351261190c6ffc4e95cc7de4d903",
		"0xa52a4adc54aae691b36aab3dae4f2cd04052d66f",
		"0x872120e7b66f106e4eaacc0e154fe6895772ae42",
		"0x5a333c5aaed62aa473e404169f9f8f989be5dbfd",
		"0x4acb6c4321253548a7d4bb9c84032cc4ee04bfd7",
		"0x08f68110f1e0ca67c80a24b4bd206675610f445d",
		"0x707d3f011ffd7eb52f0b9b4ce833913bf80e2dc3",
		"0xc987d503a9f78f6d1d782c1fff5af4cb34437e3f",
		"0x2796317b0ff8538f253012862c06787adfb8ceb6",
		"0x7db1187784db6008dd6ee213cd570a6b93969fe5",
	})
}

// Excluded: accounts with veLISTA locks or bare wallet slisBNB (out-of-scope
// "Locked"/"Staked" items), collateral DeBank decomposes into multiple tokens
// (Wombat-style SmartLP), the Moolah vault contracts themselves, and two accounts whose
// eight-figure interest-bearing balances accrue past the 10 ppm tolerance between
// DeBank's item refresh and the pinned block.
func TestListaSweepDeBankAPIReconciliation(t *testing.T) {
	if os.Getenv("PORTFOLIO_DEBANK_API_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEBANK_API_LIVE_TEST=1 to compare with DeBank Pro API")
	}
	// 1e-4 rather than the usual 1e-5: BSC Moolah borrow rates accrue interest per 0.75 s
	// block, so DeBank's per-item cache lag alone drifts 1e-5–3e-5 on any active account.
	// Structural regressions (a rotted market or vault list) fail as whole missing tokens,
	// far outside either bound.
	runDeBankAPIReconciliationRefreshedWithTolerance(t, "bsc_helio", newListaAdapter(
		liveSentioIndexerConfig(t, "PORTFOLIO_LISTA_INDEXER"),
	), BSC, []string{
		"0x8a06ac91265dbebe6d4606f45b10993e9a571869",
		"0x899dbbfd8c746d2716312196a41d3151320fe766",
		"0xc4ae5142652bd4d76f5f01b866f8e53aa97dfb2d",
		"0xf345510adf4e023afa2f61d5f712407327ea33f1",
		"0x1cd026ea8b42a376abc5d471fd8d6276a7913b93",
		"0xc80b0164365da97b4c3eac196d2466b5deabb848",
		"0x24cc9bb9c6be0b56a98508c51bc767844c2d86e6",
		"0x82f40320538d7d268b6a55a7e8364f48028eb220",
		"0x3edd122bbceba204c0a547bf889c1cd679527d87",
		"0x05cde020581a82101d46c6570edb3badeb0f8a9c",
		"0x876cda4b4e2c9c6ddcf8b8ffa084408ddda8dcc8",
		"0x1f5bd76d597c7fd18848c7b89c6b26f23e4e19ee",
		"0x57134a64b7cd9f9eb72f8255a671f5bf2fe3e2d0",
		"0x87c7e8468c5d88ef3205a72043c432291d206443",
		"0x14d0fc3be5ab5bc7c139d373f94a064ca070ab8b",
		"0x2363096a9f449c74b30e7e030ddc676f928134b2",
		"0x8be4d13014f2ea6f4d119652fe06050973ff7a50",
		"0x0458edb2418e83157f470f38c9875df10fc731e8",
		"0xc2aaaecbc0ea098eef864b30e1f788a3342ad7de",
		"0x6cccb5ffaad0fa063b744839c636079ef3bf0e00",
	}, 1e-4)
}
