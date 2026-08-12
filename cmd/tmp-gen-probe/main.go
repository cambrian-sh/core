package main

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/cambrian-sh/core/api/proto"
)

func main() {
	conn, err := grpc.NewClient("127.0.0.1:50052", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	c := pb.NewOperatorConsoleClient(conn)
	ctx := context.Background()

	login, err := c.Login(ctx, &pb.LoginRequest{Username: "operator", Password: os.Getenv("CAMBRIAN_OPERATOR_PASSWORD")})
	if err != nil {
		fmt.Println("login:", err)
		os.Exit(1)
	}
	actx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+login.Token)

	save, err := c.SaveGenerator(actx, &pb.SaveGeneratorOpRequest{
		CommandId: "probe-save-1", Reason: "kernel-path probe",
		Generator: &pb.GeneratorSpecOp{Id: "probe-model", Provider: "openai", Model: "probe-m", Endpoint: "http://localhost:9/v1"},
	})
	if err != nil {
		fmt.Println("SaveGenerator RPC error:", err)
		os.Exit(1)
	}
	for _, o := range save.Outcomes {
		fmt.Printf("save outcome: key=%s stored=%v effect=%s err=%q\n", o.Key, o.Stored, o.Effect, o.Error)
	}

	key, err := c.SetGeneratorKey(actx, &pb.SetGeneratorKeyOpRequest{
		CommandId: "probe-key-1", Reason: "kernel-path probe", GeneratorId: "probe-model", Key: "sk-probe-1234",
	})
	if err != nil {
		fmt.Println("SetGeneratorKey RPC error:", err)
	} else {
		for _, o := range key.Outcomes {
			fmt.Printf("key outcome: key=%s stored=%v effect=%s err=%q\n", o.Key, o.Stored, o.Effect, o.Error)
		}
	}

	if _, err := c.RemoveGenerator(actx, &pb.RemoveGeneratorOpRequest{
		CommandId: "probe-rm-1", Reason: "probe cleanup", GeneratorId: "probe-model",
	}); err != nil {
		fmt.Println("cleanup RemoveGenerator:", err)
	} else {
		fmt.Println("cleanup: probe-model removed")
	}
}
