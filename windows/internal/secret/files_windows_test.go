package secret

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAValueIsStoredEncryptedReadAndRemoved(t *testing.T) {
	f := &Files{Dir: filepath.Join(t.TempDir(), "secrets")}
	const secret = "sk-ant-TEST-VALUE"
	if err := f.Set("anthropic", secret); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("work.key-1", "other"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.Dir, "anthropic.value"))
	if err != nil || bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("TEST-VALUE")) {
		t.Fatalf("the file holds the key in the clear (%v)", err)
	}
	if v, ok, err := f.Get("anthropic"); err != nil || !ok || v != secret {
		t.Fatalf("Get = ok %v, %v (value matches %v)", ok, err, v == secret)
	}
	if v, ok, err := f.Get("Anthropic"); err != nil || !ok || v != secret {
		t.Errorf("Get is case-sensitive: ok %v, %v", ok, err)
	}
	if err := f.Set("anthropic", "replaced"); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := f.Get("anthropic"); v != "replaced" {
		t.Error("Set did not replace the key")
	}
	if ok, err := f.Remove("anthropic"); !ok || err != nil {
		t.Errorf("Remove = %v, %v", ok, err)
	}
	if _, ok, err := f.Get("anthropic"); ok || err != nil {
		t.Errorf("Get after Remove = %v, %v", ok, err)
	}
	if ok, err := f.Remove("anthropic"); ok || err != nil {
		t.Errorf("second Remove = %v, %v", ok, err)
	}
}

func TestABlobCopiedToAnotherNameDoesNotDecrypt(t *testing.T) {
	f := &Files{Dir: t.TempDir()}
	if err := f.Set("openai", "sk-proj-TEST"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(f.Dir, "openai.value"))
	os.WriteFile(filepath.Join(f.Dir, "gemini.value"), raw, 0o600)
	if _, ok, err := f.Get("gemini"); ok || err == nil {
		t.Errorf("a renamed blob read as gemini: ok %v, %v", ok, err)
	}
}

func TestNamesAreChecked(t *testing.T) {
	for _, bad := range []string{"", "..", ".", "a/b", `a\b`, "a b", "key:1", string(make([]byte, 65))} {
		if ValidName(bad) == nil {
			t.Errorf("ValidName(%q) passed", bad)
		}
	}
	f := &Files{Dir: t.TempDir()}
	if err := f.Set("../escape", "x"); err == nil {
		t.Error("Set took a path as a name")
	}
}
