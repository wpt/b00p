package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
)

func TestPostArchive_PreservesSourceFields(t *testing.T) {
	source := `{"id":"p1","hasAccess":true,"futureMetadata":{"large":9007199254740993},"data":[{"type":"list","style":"unordered","items":[{"data":[{"type":"text","content":"keep","future":"value"}],"items":[]}]}]}`
	var post boosty.Post
	input := []byte(source)
	if err := json.Unmarshal(input, &post); err != nil {
		t.Fatal(err)
	}
	for i := range input {
		input[i] = ' '
	}
	dst := filepath.Join(t.TempDir(), "post.json")
	if err := writeJSON(dst, post.Raw, &post); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	decode := func(s string) any {
		var value any
		dec := json.NewDecoder(strings.NewReader(s))
		dec.UseNumber()
		if err := dec.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !reflect.DeepEqual(decode(string(saved)), decode(source)) {
		t.Fatalf("source fields changed or lost: %s", saved)
	}
}

func TestPostArchive_ConstructedPost(t *testing.T) {
	post := &boosty.Post{ID: "p1", Title: "Constructed"}
	dst := filepath.Join(t.TempDir(), "post.json")
	if err := writeJSON(dst, post.Raw, post); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	var got boosty.Post
	if err := json.Unmarshal(saved, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != post.ID || got.Title != post.Title {
		t.Fatalf("constructed post changed: %s", saved)
	}
}
