package portfolio

// protocolAvailabilityByID is the engine-level envelope for every registered
// protocol and chain. A protocol is runnable at the first block where at least
// one of its independently gated surfaces can execute safely. Later markets,
// vaults, rewards, factories, and replacement generations keep their narrower
// component windows inside the adapter.
//
// Blocks are closed history established with archive RPC boundary reads. For a
// proxy or registry, the boundary is the first block where the mandatory view
// used by the adapter succeeds, which can be later than the first bytecode.
var protocolAvailabilityByID = map[string]chainAvailability{
	"aave-v3": {
		Ethereum:  {availableFrom(16_291_078)},
		BSC:       {availableFrom(46_367_909)},
		Base:      {availableFrom(25_954_709)},
		Arbitrum:  {availableFrom(302_650_382)},
		Polygon:   {availableFrom(25_826_028)},
		Monad:     {availableFrom(81_909_763)},
		Plasma:    {availableFrom(489_197)},
		Avalanche: {availableFrom(11_970_506)},
		Optimism:  {availableFrom(4_365_693)},
	},
	"aave-v2": {
		Ethereum:  {availableFrom(10_927_018)},
		Polygon:   {availableFrom(12_687_302)},
		Avalanche: {availableFrom(4_607_174)},
	},
	"spark": {
		Ethereum:  {availableFrom(16_776_391)},
		Base:      {availableFrom(27_123_520)},
		Arbitrum:  {availableFrom(311_940_473)},
		Avalanche: {availableFrom(69_983_672)},
		Optimism:  {availableFrom(136_322_256)},
	},
	"kinza": {
		BSC: {availableFrom(29_232_063)},
	},
	"seamless": {
		Base: {availableFrom(3_318_562)},
	},
	"compound-v2": {
		Ethereum: {availableFrom(10_271_924)},
	},
	"moonwell": {
		Base:     {availableFrom(2_162_402)},
		Optimism: {availableFrom(122_531_304)},
	},
	"flux-finance": {
		Ethereum: {availableFrom(16_520_940)},
	},
	"sonne": {
		Base:     {availableFrom(2_492_954)},
		Optimism: {availableFrom(25_840_175)},
	},
	"lodestar": {
		Arbitrum: {availableFrom(111_013_008)},
	},
	"venus": {
		Ethereum: {availableFrom(18_890_246)},
		BSC:      {availableFrom(2_471_694)},
		Base:     {availableFrom(23_341_263)},
		Arbitrum: {availableFrom(215_551_349)},
		Optimism: {availableFrom(126_048_098)},
	},
	"compound-v3": {
		Ethereum: {availableFrom(15_331_586)},
		Base:     {availableFrom(2_197_588)},
		Arbitrum: {availableFrom(87_335_214)},
		Polygon:  {availableFrom(39_412_367)},
		Optimism: {availableFrom(118_406_276)},
	},
	"fluid-lite": {
		Ethereum: {availableFrom(16_609_585)},
	},
	"cap": {
		Ethereum: {availableFrom(22_874_057)},
	},
	"ethena": {
		Ethereum: {availableFrom(18_571_359)},
	},
	"usdd": {
		Ethereum: {availableFrom(23_275_147)},
		BSC:      {availableFrom(63_887_220)},
	},
	"unitas": {
		BSC: {availableFrom(69_059_010)},
	},
	"liquid-collective": {
		Ethereum: {availableFrom(15_676_402)},
	},
	"lido": {
		Ethereum: {availableFrom(11_473_216)},
	},
	"meth-protocol": {
		Ethereum: {availableFrom(18_290_599)},
	},
	"etherfi": {
		Ethereum: {availableFrom(17_664_324)},
		BSC:      {availableFrom(38_098_558)},
		Base:     {availableFrom(13_524_685)},
		Arbitrum: {availableFrom(156_547_814)},
		Optimism: {availableFrom(120_917_167)},
	},
	"frax-ether": {
		Ethereum: {availableFrom(15_686_046)},
	},
	"renzo": {
		Ethereum: {availableFrom(18_722_779)},
		BSC:      {availableFrom(36_596_546)},
		Base:     {availableFrom(12_682_160)},
		Arbitrum: {availableFrom(185_410_162)},
	},
	"aster": {
		BSC: {availableFrom(43_713_424)},
	},
	"fxprotocol": {
		Ethereum: {availableFrom(17_818_955)},
	},
	"rocketpool": {
		Ethereum: {availableFrom(13_325_532)},
	},
	"stader": {
		Ethereum: {availableFrom(17_416_153)},
		BSC:      {availableFrom(19_907_065)},
		Polygon:  {availableFrom(staderPolygonDeployment.tokenActivationBlock)},
	},
	"olympus": {
		Ethereum: {availableFrom(13_803_969)},
	},
	"fraxlend": {
		Ethereum: {availableFrom(15_993_000)},
	},
	"aave-v4": {
		Ethereum:  {availableFrom(24_720_887)},
		Avalanche: {availableFrom(89_721_368)},
	},
	"makerdao": {
		Ethereum: {availableFrom(10_091_068)},
	},
	"sky": {
		Ethereum: {availableFrom(20_677_434)},
	},
	"maple": {
		Ethereum: {availableFrom(16_162_315)},
	},
	"liquity-v1": {
		Ethereum: {availableFrom(12_178_557)},
	},
	"crvusd": {
		Ethereum: {availableFrom(17_257_955)},
	},
	"curve-lending": {
		Ethereum: {availableFrom(19_422_660)},
		Arbitrum: {availableFrom(193_652_535)},
		Optimism: {availableFrom(125_072_267)},
	},
	"vesper": {
		Ethereum:  {availableFrom(11_407_993)},
		Base:      {availableFrom(15_153_629)},
		Avalanche: {availableFrom(9_452_892)},
		Optimism:  {availableFrom(75_560_596)},
	},
	"yearn-v3": {
		Ethereum: {availableFrom(18_817_046)},
		Base:     {availableFrom(17_834_110)},
		Arbitrum: {availableFrom(173_129_408)},
		Polygon:  {availableFrom(48_951_031)},
	},
	"beefy": {
		Ethereum:  {availableFrom(15_982_782)},
		BSC:       {availableFrom(1_174_856)},
		Base:      {availableFrom(2_572_135)},
		Arbitrum:  {availableFrom(3_005_534)},
		Polygon:   {availableFrom(14_272_076)},
		Monad:     {availableFrom(38_165_449)},
		Plasma:    {availableFrom(2_013_105)},
		Avalanche: {availableFrom(3_052_900)},
		Optimism:  {availableFrom(17_722_021)},
	},
	"stakewise": {
		Ethereum: {availableFrom(18_470_152)},
	},
	"lista": {
		Ethereum: {availableFrom(23_445_769)},
		BSC:      {availableFrom(20_324_823)},
	},
	"euler-v2": {
		Ethereum:  {availableFrom(20_529_207)},
		BSC:       {availableFrom(46_370_645)},
		Base:      {availableFrom(22_282_353)},
		Arbitrum:  {availableFrom(300_690_886)},
		Polygon:   {availableFrom(86_932_963)},
		Monad:     {availableFrom(30_858_592)},
		Plasma:    {availableFrom(511_021)},
		Avalanche: {availableFrom(56_805_710)},
	},
	"morpho-blue": {
		Ethereum:  {availableFrom(18_883_124)},
		BSC:       {availableFrom(54_344_680)},
		Base:      {availableFrom(13_977_148)},
		Arbitrum:  {availableFrom(296_446_593)},
		Polygon:   {availableFrom(66_931_042)},
		Monad:     {availableFrom(31_907_457)},
		Plasma:    {availableFrom(2_919_883)},
		Avalanche: {availableFrom(75_313_888)},
		Optimism:  {availableFrom(130_770_075)},
	},
	"pendle": {
		Ethereum: {availableFrom(16_032_048)},
		BSC:      {availableFrom(29_484_198)},
		Base:     {availableFrom(22_350_319)},
		Arbitrum: {availableFrom(62_977_844)},
		Monad:    {availableFrom(75_580_673)},
		Plasma:   {availableFrom(1_887_231)},
		Optimism: {availableFrom(108_061_318)},
	},
	"fluid": {
		Ethereum: {availableFrom(19_245_687)},
		BSC:      {availableFrom(71_737_128)},
		Base:     {availableFrom(38_678_564)},
		Arbitrum: {availableFrom(228_709_698)},
		Polygon:  {availableFrom(79_090_648)},
		Plasma:   {availableFrom(8_682_622)},
	},
	"uniswap-v3": {
		Ethereum:  {availableFrom(12_369_651)},
		BSC:       {availableFrom(26_324_045)},
		Base:      {availableFrom(1_371_714)},
		Arbitrum:  {availableFrom(173)},
		Polygon:   {availableFrom(22_760_586)},
		Monad:     {availableFrom(29_255_879)},
		Plasma:    {availableFrom(430_178)},
		Avalanche: {availableFrom(27_833_025)},
		Optimism:  {availableFromGenesis()},
	},
	"uniswap-v4": {
		Ethereum:  {availableFrom(21_689_089)},
		BSC:       {availableFrom(45_970_613)},
		Base:      {availableFrom(25_350_993)},
		Arbitrum:  {availableFrom(297_842_893)},
		Polygon:   {availableFrom(66_980_399)},
		Monad:     {availableFrom(29_255_924)},
		Avalanche: {availableFrom(56_195_389)},
		Optimism:  {availableFrom(130_947_685)},
	},
	"wallet": {
		Ethereum:  {availableFromGenesis()},
		BSC:       {availableFromGenesis()},
		Base:      {availableFromGenesis()},
		Arbitrum:  {availableFromGenesis()},
		Polygon:   {availableFromGenesis()},
		Monad:     {availableFromGenesis()},
		Plasma:    {availableFromGenesis()},
		Avalanche: {availableFromGenesis()},
		Optimism:  {availableFromGenesis()},
	},
}
