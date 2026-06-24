package plugin_test

import (
	"context"
	"testing"

	"github.com/tight-line/ballast/internal/plugin"
)

type fakePlugin struct{ typeName string }

func (f *fakePlugin) Type() string { return f.typeName }
func (f *fakePlugin) FetchStats(_ context.Context, _ plugin.WorkloadIdentity, _ plugin.TimeWindow) ([]plugin.ContainerStats, error) {
	return nil, nil
}

func TestRegisterAndGet(t *testing.T) {
	fp := &fakePlugin{typeName: "fake-" + t.Name()}
	plugin.Register(fp)

	got, ok := plugin.Get("fake-" + t.Name())
	if !ok {
		t.Fatal("expected registered plugin to be found")
	}
	if got.Type() != fp.typeName {
		t.Errorf("Type() = %q, want %q", got.Type(), fp.typeName)
	}
}

func TestGet_Missing(t *testing.T) {
	_, ok := plugin.Get("nonexistent-" + t.Name())
	if ok {
		t.Error("expected false for unregistered plugin type")
	}
}

func TestRegister_Overwrite(t *testing.T) {
	name := "overwrite-" + t.Name()
	plugin.Register(&fakePlugin{typeName: name})
	second := &fakePlugin{typeName: name}
	plugin.Register(second)

	got, ok := plugin.Get(name)
	if !ok {
		t.Fatal("expected plugin after re-registration")
	}
	if got != second {
		t.Error("expected second registration to win")
	}
}
