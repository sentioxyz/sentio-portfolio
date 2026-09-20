package portfolio

import (
	"encoding/binary"
	"golang.org/x/crypto/blake2b"
)

func suilendHistoryType(kind string) string {
	switch kind {
	case "cap":
		return suiType(suilendCapType)
	case "obligation":
		return suiType(suilendPackage + "::obligation::Obligation")
	case "market":
		return suiType(suilendPackage + "::lending_market::LendingMarket")
	}
	return ""
}
func suilendObligationParent(table, obligation string) (string, error) {
	parent, err := ParseSuiAddress(table)
	if err != nil {
		return "", err
	}
	id, err := ParseSuiAddress(obligation)
	if err != nil {
		return "", err
	}
	// BCS TypeTag::Struct(0x2::dynamic_object_field::Wrapper<0x2::object::ID>).
	// Keep this exact layout separate from CLMM's non-generic key admission.
	framework, _ := ParseSuiAddress("0x2")
	tag := append([]byte{7}, framework[:]...)
	tag = append(tag, 20)
	tag = append(tag, "dynamic_object_field"...)
	tag = append(tag, 7)
	tag = append(tag, "Wrapper"...)
	tag = append(tag, 1, 7)
	tag = append(tag, framework[:]...)
	tag = append(tag, 6)
	tag = append(tag, "object"...)
	tag = append(tag, 2, 'I', 'D', 0)
	data := append([]byte{0xf0}, parent[:]...)
	data = binary.LittleEndian.AppendUint64(data, uint64(len(id)))
	data = append(data, id[:]...)
	data = append(data, tag...)
	hash := blake2b.Sum256(data)
	return SuiAddress(hash).Hex(), nil
}

const suilendHistoryStart uint64 = 28510257

// SuilendHistoryReader reads completed hourly lending state from its processor
// index, including ownership, market reserves, obligations and coin metadata.
type SuilendHistoryReader struct{ *suiHistoryIndex }

func NewSuilendHistoryReader(config SentioIndexerConfig) (*SuilendHistoryReader, error) {
	i, err := newSuiHistoryIndex(config, "suilend", "Suilend", suilendHistoryStart)
	if err != nil {
		return nil, err
	}
	return &SuilendHistoryReader{i}, nil
}

func (r *SuilendHistoryReader) WithEngine(engine *Engine) *SuilendHistoryReader {
	return &SuilendHistoryReader{r.suiHistoryIndex.WithEngine(engine)}
}
