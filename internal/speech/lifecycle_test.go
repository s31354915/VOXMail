package speech

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareStaticPromptsRefreshesWhenModelChanges(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "voice.onnx")
	if err := os.WriteFile(model, []byte("model-one"), 0600); err != nil {
		t.Fatal(err)
	}
	piper := filepath.Join(root, "fake-piper.sh")
	script := "#!/bin/sh\nout=\nnext=0\nfor arg in \"$@\"; do if [ \"$next\" = 1 ]; then out=$arg; next=0; elif [ \"$arg\" = \"--output_file\" ]; then next=1; fi; done\nprintf 'RIFFstatic' > \"$out\"\n"
	if err := os.WriteFile(piper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	greeting := filepath.Join(root, "prompts", "welcome.wav")
	main := filepath.Join(root, "prompts", "main-menu.wav")
	manifest := filepath.Join(root, "prompts", "static-prompts.json")
	p := Piper{Binary: piper, Model: model}
	first, err := PrepareStaticPrompts(context.Background(), p, greeting, main, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if first.ModelSHA256 == "" {
		t.Fatal("missing model digest")
	}
	if _, err := PrepareStaticPrompts(context.Background(), p, greeting, main, manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("model-two"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := PrepareStaticPrompts(context.Background(), p, greeting, main, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if first.ModelSHA256 == second.ModelSHA256 {
		t.Fatal("static prompt manifest did not refresh after model change")
	}
}
