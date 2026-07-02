package network

import (
	"context"
	"errors"
	"testing"

	pb "github.com/cambrian-sh/cambrian-runtime/api/proto"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type stubEmbedder struct {
	vec []float32
	err error
}

func (s stubEmbedder) Embed(context.Context, string) ([]float32, error) { return s.vec, s.err }

func TestEmbed_ReturnsVectorFromKernelEmbedder(t *testing.T) {
	srv := &Server{Embedder: stubEmbedder{vec: []float32{0.1, 0.2, 0.3}}}
	resp, err := srv.Embed(context.Background(), &pb.EmbedRequest{Text: "hello"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(resp.Vector) != 3 || resp.Vector[0] != 0.1 {
		t.Errorf("vector = %v, want [0.1 0.2 0.3]", resp.Vector)
	}
}

func TestEmbed_NilEmbedderUnimplemented(t *testing.T) {
	srv := &Server{}
	if _, err := srv.Embed(context.Background(), &pb.EmbedRequest{Text: "x"}); status.Code(err) != codes.Unimplemented {
		t.Errorf("nil embedder must be Unimplemented, got %v", err)
	}
}

func TestEmbed_EmbedderErrorPropagated(t *testing.T) {
	srv := &Server{Embedder: stubEmbedder{err: errors.New("model down")}}
	if _, err := srv.Embed(context.Background(), &pb.EmbedRequest{Text: "x"}); status.Code(err) != codes.Internal {
		t.Errorf("embedder error must map to Internal, got %v", err)
	}
}
