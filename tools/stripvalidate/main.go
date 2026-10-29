// Command stripvalidate sits between `buf build` and `buf generate` in `task gen-ts`:
//
//	buf build -o - | go run ./tools/stripvalidate | buf generate --template buf.gen.ts.yaml --include-imports -
//
// It reads a buf image on stdin and writes the stripped image on stdout (see Strip).
package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "stripvalidate:", err)
		os.Exit(1)
	}
}

func run(in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read image: %w", err)
	}

	// A buf image is a FileDescriptorSet plus buf-only extension fields; those land in
	// unknown fields and survive the round trip (is_import decides what --include-imports emits).
	set := &descriptorpb.FileDescriptorSet{}
	if err := proto.Unmarshal(raw, set); err != nil {
		return fmt.Errorf("parse image: %w", err)
	}
	if len(set.GetFile()) == 0 {
		return errors.New("image has no files (expected the output of `buf build -o -`)")
	}
	if err := Strip(set); err != nil {
		return err
	}

	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(set)
	if err != nil {
		return fmt.Errorf("marshal image: %w", err)
	}
	if _, err := out.Write(b); err != nil {
		return fmt.Errorf("write image: %w", err)
	}
	return nil
}
