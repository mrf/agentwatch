package session

import (
	"errors"
	"testing"
)

func TestNewPseudonymizer_ShortKeyIsErrPseudonymKeyTooShort(t *testing.T) {
	_, err := NewPseudonymizer(WithPseudonymKey(make([]byte, pseudonymKeySize-1)))
	if !errors.Is(err, errPseudonymKeyTooShort) {
		t.Fatalf("NewPseudonymizer error = %v, want errPseudonymKeyTooShort", err)
	}
}
