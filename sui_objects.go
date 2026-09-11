package portfolio

import "context"

// SuiObject is the latest live version of a Move object. OwnerKind distinguishes
// a wallet-owned capability from a child object with the same owner address.
type SuiObject struct {
	ID, ObjectType, Owner, OwnerKind, Content string
	Version                                   uint64
}

// SuiObjectReader reads current object state, without substituting it for a
// historical checkpoint. Missing IDs in Objects mean an explicit NotFound.
type SuiObjectReader interface {
	SuiReader
	Objects(context.Context, []string) (map[string]SuiObject, error)
	OwnedObjects(context.Context, SuiAddress, string) ([]SuiObject, error)
	DynamicFields(context.Context, string) ([]SuiObject, error)
}

// SuiObjectDirectory discovers shared objects by their defining Move type.
// Directory entries are candidates: their current type and contents are always
// checked through SuiObjectReader before they affect portfolio quantities.
type SuiObjectDirectory interface {
	ObjectsByType(context.Context, string) ([]string, error)
}
