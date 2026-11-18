package gateway

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestAuthorizerGatesWrites (C-008): every write kind is put to the
// authorizer before anything is queued or sent; a refusal is returned
// as-is and nothing is injected. Reads are never gated.
func TestAuthorizerGatesWrites(t *testing.T) {
	clk := &vclock{now: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	refused := errors.New("refused")
	var asked []CommandKind
	inj := &spyInjector{}
	gw := New(inj, clk.get, WithAuthorizer(func(c Command) error {
		asked = append(asked, c.Kind)
		return refused
	}))
	gw.SetFlow("c1")
	writes := []Command{
		{Kind: KindWorkMode, Params: map[Param]int{pMode: 1}},
		{Kind: KindMeterCompensation, Params: map[Param]int{pValue: -50}},
		{Kind: KindLimits, Params: map[Param]int{pExport: 5000}},
		baseSchedule(),
	}
	for _, c := range writes {
		if _, err := gw.Submit(context.Background(), c); !errors.Is(err, refused) {
			t.Errorf("%s: %v, want the authorizer's refusal", c.Kind, err)
		}
	}
	if n := len(inj.injected()); n != 0 {
		t.Fatalf("refused writes injected %d frames", n)
	}
	if len(asked) != len(writes) {
		t.Fatalf("authorizer asked %v, want the four write kinds", asked)
	}
	read := readCmd(3, 31000, 29)
	read.Origin = OriginPoll
	if _, err := gw.Submit(context.Background(), read); err != nil {
		t.Fatalf("read refused: %v", err)
	}
	if len(asked) != len(writes) {
		t.Fatalf("a read was put to the authorizer")
	}
}
