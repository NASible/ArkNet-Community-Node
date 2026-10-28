package media

import (
	"errors"
	"strings"
	"testing"
)

func TestGenerateMediaIDIsOpaqueAndValid(t *testing.T) {
	first, err := GenerateMediaID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateMediaID()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidMediaID(first) || !ValidMediaID(second) || first == second {
		t.Fatalf("generated IDs are not distinct valid opaque IDs: %q %q", first, second)
	}
}

func TestValidateMediaObject(t *testing.T) {
	valid := MediaObject{ObjectType: ObjectTypeVideo, State: ObjectActive, InfoHashV2: strings.Repeat("a", InfoHashV2Length)}
	for _, test := range []struct {
		name   string
		mutate func(*MediaObject)
		want   error
	}{
		{"negative size", func(object *MediaObject) { object.Size = -1 }, ErrInvalidSize},
		{"unknown type", func(object *MediaObject) { object.ObjectType = "BOOK" }, ErrInvalidObjectType},
		{"unknown state", func(object *MediaObject) { object.State = "DELETED" }, ErrInvalidState},
		{"uppercase hash", func(object *MediaObject) { object.InfoHashV2 = strings.Repeat("A", InfoHashV2Length) }, ErrInvalidInfoHashV2},
	} {
		t.Run(test.name, func(t *testing.T) {
			object := valid
			test.mutate(&object)
			if err := Validate(object); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
	if err := Validate(valid); err != nil {
		t.Fatalf("valid object rejected: %v", err)
	}
}
