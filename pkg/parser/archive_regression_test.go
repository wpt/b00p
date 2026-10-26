package parser

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wpt/b00p/pkg/boosty"
)

func TestArchiveMarkdown_HeadingsListsAndMediaOrder(t *testing.T) {
	var post boosty.Post
	err := json.Unmarshal([]byte(`{"title":"Example","data":[
        {"type":"text","content":"Before"},
        {"type":"image","url":"https://example.com/a.png"},
        {"type":"header","content":"[\"Section\",\"header-two\",[]]"},
        {"type":"list","style":"unordered","items":[
            {"data":[{"type":"text","content":"[\"Read \",\"unstyled\",[]]"},{"type":"link","content":"this","url":"https://example.com"}],"items":[
                {"data":[{"type":"text","content":"Nested"}],"items":[]}
            ]},
            {"data":[{"type":"text","content":"Second"}],"items":[]}
        ]},
        {"type":"ok_video","playerUrls":[{"type":"high","url":"https://example.com/v.mp4"}]},
        {"type":"header","content":"[\"Details\",\"header-three\",[]]"},
        {"type":"text","content":"After"}
    ]}`), &post)
	if err != nil {
		t.Fatal(err)
	}
	parsed := ParseBlocks(post.Data)
	md := GenerateMarkdown(&post, parsed)
	want := "Before\n\n![image_001.png](image_001.png)\n\n## Section\n\n- Read [this](<https://example.com>)\n  - Nested\n- Second\n\n[Video: video_001.mp4](video_001.mp4)\n\n### Details\n\nAfter\n\n"
	if !strings.HasSuffix(md, want) {
		t.Fatalf("lost content or order:\n%s", md)
	}
	if len(parsed.Media) != 2 {
		t.Fatalf("media = %d, want 2", len(parsed.Media))
	}
	if len(parsed.UnknownTypes) != 0 {
		t.Errorf("known blocks flagged: %v", parsed.UnknownTypes)
	}
}

func TestArchiveMarkdown_ListMediaKeepUniqueFilenames(t *testing.T) {
	var post boosty.Post
	err := json.Unmarshal([]byte(`{"data":[
        {"type":"image","url":"https://example.com/one.jpg"},
        {"type":"list","style":"ordered","items":[
            {"data":[{"type":"text","content":"First"},{"type":"image","url":"https://example.com/two.jpg"}]},
            {"data":[{"type":"text","content":"Second"}]}
        ]},
        {"type":"image","url":"https://example.com/three.jpg"}
    ]}`), &post)
	if err != nil {
		t.Fatal(err)
	}
	parsed := ParseBlocks(post.Data)
	md := GenerateMarkdown(&post, parsed)
	want := "![image_001.jpg](image_001.jpg)\n\n1. First\n\n   ![image_002.jpg](image_002.jpg)\n2. Second\n\n![image_003.jpg](image_003.jpg)\n\n"
	if !strings.HasSuffix(md, want) {
		t.Fatalf("list/media order lost:\n%s", md)
	}
	if len(parsed.Media) != 3 {
		t.Fatalf("media = %d, want 3", len(parsed.Media))
	}
	for i, name := range []string{"image_001.jpg", "image_002.jpg", "image_003.jpg"} {
		if parsed.Media[i].Filename != name {
			t.Errorf("media[%d] = %s, want %s", i, parsed.Media[i].Filename, name)
		}
	}
}
