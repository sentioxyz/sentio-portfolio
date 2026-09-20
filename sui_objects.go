package portfolio

import "context"

// SuiObject is a version of a Move object. OwnerKind distinguishes
// a wallet-owned capability from a child object with the same owner address.
type SuiObject struct {
	ID, ObjectType, Owner, OwnerKind, Content string
	Digest                                    string
	Version                                   uint64
	PreviousTransaction                       string
}

// SuiObjectReader reads current object state, without substituting it for a
// historical checkpoint. Missing IDs in Objects mean an explicit NotFound.
type SuiObjectReader interface {
	SuiReader
	Objects(context.Context, []string) (map[string]SuiObject, error)
	OwnedObjects(context.Context, SuiAddress, string) ([]SuiObject, error)
	DynamicFields(context.Context, string) ([]SuiObject, error)
}
