package authn

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeKeyFile(t *testing.T, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	// WriteFile honours umask, so force the mode the test asked for.
	if err := os.Chmod(p, mode); err != nil {
		t.Fatalf("chmod %s: %v", name, err)
	}
	return p
}

func TestLoadKeyStore_JSONAndYAML(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content string
	}{
		{"json", "keys.json", `{"key-a1":"acme","key-a2":"acme","key-b1":"beta"}`},
		{"yaml", "keys.yaml", "key-a1: acme\nkey-a2: acme\nkey-b1: beta\n"},
		{"yml", "keys.yml", "key-a1: acme\nkey-a2: acme\nkey-b1: beta\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := LoadKeyStore(writeKeyFile(t, tc.file, tc.content, 0o600))
			if err != nil {
				t.Fatalf("LoadKeyStore: %v", err)
			}
			if store.Len() != 3 {
				t.Fatalf("Len=%d, want 3", store.Len())
			}
			// Multiple keys per tenant is the rotation story from #198 Q2.
			for _, k := range []string{"key-a1", "key-a2"} {
				if tenant, ok := store.Lookup(k); !ok || tenant != "acme" {
					t.Errorf("Lookup(%s) = (%q,%v), want (acme,true)", k, tenant, ok)
				}
			}
			if tenant, ok := store.Lookup("key-b1"); !ok || tenant != "beta" {
				t.Errorf("Lookup(key-b1) = (%q,%v), want (beta,true)", tenant, ok)
			}
			if _, ok := store.Lookup("key-a1 "); ok {
				t.Error("a near-miss key must not authenticate")
			}
			if got := store.Tenants(); len(got) != 2 || got[0] != "acme" || got[1] != "beta" {
				t.Errorf("Tenants() = %v, want [acme beta]", got)
			}
		})
	}
}

// A key file readable or writable by group/other is refused, and the refusal
// names the actual mode so the operator knows what to chmod.
func TestLoadKeyStore_RefusesPermissivePermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660, 0o666} {
		path := writeKeyFile(t, "keys.json", `{"k":"acme"}`, mode)
		_, err := LoadKeyStore(path)
		if err == nil {
			t.Fatalf("mode %04o: expected refusal", mode)
		}
		if !strings.Contains(err.Error(), "too permissive") {
			t.Errorf("mode %04o: error %q should explain the permission problem", mode, err)
		}
		if !strings.Contains(err.Error(), "0"+strings.TrimPrefix(modeString(mode), "0")) {
			t.Errorf("mode %04o: error %q should quote the actual mode", mode, err)
		}
	}
}

func modeString(m os.FileMode) string {
	const digits = "01234567"
	v := uint32(m.Perm())
	return string([]byte{'0', digits[(v>>6)&7], digits[(v>>3)&7], digits[v&7]})
}

func TestLoadKeyStore_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		content string
		want    string
	}{
		{"duplicate json key", "keys.json", `{"qzsecretzq":"acme","qzsecretzq":"beta"}`, "duplicate key"},
		{"duplicate yaml key", "keys.yaml", "dup: acme\ndup: beta\n", "duplicate key"},
		{"empty key", "keys.json", `{"":"acme"}`, "empty key"},
		{"whitespace key", "keys.json", `{"a b":"acme"}`, "whitespace"},
		{"empty tenant", "keys.json", `{"k":""}`, "invalid tenant"},
		{"blank tenant", "keys.json", `{"k":"   "}`, "invalid tenant"},
		{"control char tenant", "keys.json", "{\"k\":\"ac\\u0000me\"}", "invalid tenant"},
		{"oversize tenant", "keys.json", `{"k":"` + strings.Repeat("t", 200) + `"}`, "invalid tenant"},
		{"non-string tenant", "keys.json", `{"k":7}`, "must be a string"},
		{"not an object", "keys.json", `["k"]`, "object"},
		{"empty file", "keys.json", "  \n", "empty"},
		{"unknown extension", "keys.txt", `{"k":"acme"}`, "unsupported extension"},
		{"malformed json", "keys.json", `{"k":`, "invalid JSON"},
		{"yaml sequence", "keys.yaml", "- a\n- b\n", "mapping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadKeyStore(writeKeyFile(t, tc.file, tc.content, 0o600))
			if err == nil {
				t.Fatalf("expected refusal for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
			// The key itself must never be echoed back — the position is
			// enough to find it.
			if strings.Contains(err.Error(), "qzsecretzq") {
				t.Errorf("error %q leaks key material", err)
			}
		})
	}
}

func TestLoadKeyStore_MissingFileAndEmptyPath(t *testing.T) {
	if store, err := LoadKeyStore(""); err != nil || store != nil {
		t.Fatalf("empty path: got (%v, %v), want (nil, nil)", store, err)
	}
	if _, err := LoadKeyStore(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("missing file must be refused")
	}
	if store, _ := LoadKeyStore(""); store.Enabled() {
		t.Error("nil store must report Enabled() == false")
	}
}

// TestKeyStore_DigestOnlyMemory walks the loaded store with reflection and
// asserts the raw key string appears nowhere in it. Digests only — a heap dump
// or a stray %+v must not yield a usable bearer token.
func TestKeyStore_DigestOnlyMemory(t *testing.T) {
	const secret = "super-secret-key-value"
	store, err := LoadKeyStore(writeKeyFile(t, "keys.json", `{"`+secret+`":"acme"}`, 0o600))
	if err != nil {
		t.Fatalf("LoadKeyStore: %v", err)
	}
	if tenant, ok := store.Lookup(secret); !ok || tenant != "acme" {
		t.Fatalf("Lookup = (%q,%v), want (acme,true)", tenant, ok)
	}
	forEachString(t, reflect.ValueOf(store), func(path, s string) {
		if strings.Contains(s, secret) {
			t.Fatalf("raw key retained at %s", path)
		}
	})
	// The digest IS retained — otherwise the walk above proves nothing.
	want := sha256.Sum256([]byte(secret))
	if store.entries[0].digest != want {
		t.Fatal("store does not hold the key digest")
	}
}

// forEachString visits every string reachable from v.
func forEachString(t *testing.T, v reflect.Value, fn func(path, s string)) {
	t.Helper()
	var walk func(reflect.Value, string)
	walk = func(v reflect.Value, path string) {
		if !v.IsValid() {
			return
		}
		switch v.Kind() {
		case reflect.String:
			fn(path, v.String())
		case reflect.Ptr, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem(), path+".*")
			}
		case reflect.Slice, reflect.Array:
			for i := range v.Len() {
				walk(v.Index(i), path+"[]")
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				walk(k, path+".key")
				walk(v.MapIndex(k), path+".value")
			}
		case reflect.Struct:
			for i := range v.NumField() {
				walk(v.Field(i), path+"."+v.Type().Field(i).Name)
			}
		}
	}
	walk(v, "store")
}

func TestNewKeyStoreFromMap_SameValidation(t *testing.T) {
	if _, err := NewKeyStoreFromMap(map[string]string{"k": ""}); err == nil {
		t.Error("empty tenant must be refused")
	}
	if _, err := NewKeyStoreFromMap(nil); err == nil {
		t.Error("empty map must be refused")
	}
	store, err := NewKeyStoreFromMap(map[string]string{"k": "acme"})
	if err != nil {
		t.Fatalf("NewKeyStoreFromMap: %v", err)
	}
	if tenant, ok := store.Lookup("k"); !ok || tenant != "acme" {
		t.Fatalf("Lookup = (%q,%v)", tenant, ok)
	}
}
