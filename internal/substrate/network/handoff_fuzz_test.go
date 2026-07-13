package network

import (
	"testing"

	"github.com/cambrian-sh/core/domain"
	pb "github.com/cambrian-sh/core/api/proto"
	"google.golang.org/protobuf/proto"
)

func FuzzProtoToHandoff(f *testing.F) {
	// Seed 1: Valid full Handoff
	valid := &pb.Handoff{
		Id:        "handoff-001",
		FromAgent: "agent-a",
		ToAgent:   "agent-b",
		Payload: &pb.Object{
			Id:   "payload-001",
			Type: "text",
			Data: []byte("hello"),
		},
		Confidence: 0.85,
	}
	validBytes, err := proto.Marshal(valid)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(validBytes)

	// Seed 2: Nil Payload
	nilPayload := &pb.Handoff{
		Id:        "handoff-002",
		FromAgent: "agent-a",
		ToAgent:   "agent-b",
	}
	nilPayloadBytes, err := proto.Marshal(nilPayload)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(nilPayloadBytes)

	// Seed 3: Confidence = -1 (out of range)
	negConf := &pb.Handoff{
		Id:         "handoff-003",
		FromAgent:  "agent-a",
		ToAgent:    "agent-b",
		Payload:    &pb.Object{Id: "p", Type: "text", Data: []byte("x")},
		Confidence: -1.0,
	}
	negConfBytes, err := proto.Marshal(negConf)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(negConfBytes)

	// Seed 4: Confidence = 2.0 (out of range)
	overConf := &pb.Handoff{
		Id:         "handoff-004",
		FromAgent:  "agent-a",
		ToAgent:    "agent-b",
		Payload:    &pb.Object{Id: "p", Type: "text", Data: []byte("x")},
		Confidence: 2.0,
	}
	overConfBytes, err := proto.Marshal(overConf)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(overConfBytes)

	// Seed 5: Empty required fields
	emptyFields := &pb.Handoff{
		Id:        "",
		FromAgent: "",
		ToAgent:   "",
		Payload:   &pb.Object{Id: "", Type: "", Data: []byte{}},
	}
	emptyFieldsBytes, err := proto.Marshal(emptyFields)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(emptyFieldsBytes)

	// Seed 6: Maximum-length strings
	longStr := make([]byte, 65536)
	for i := range longStr {
		longStr[i] = 'x'
	}
	maxLength := &pb.Handoff{
		Id:        string(longStr[:4096]),
		FromAgent: string(longStr[:4096]),
		ToAgent:   string(longStr[:4096]),
		Payload: &pb.Object{
			Id:   string(longStr[:4096]),
			Type: string(longStr[:4096]),
			Data: longStr,
		},
	}
	maxLengthBytes, err := proto.Marshal(maxLength)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(maxLengthBytes)

	// Seed 7: Unknown payload type
	unknownType := &pb.Handoff{
		Id:        "handoff-007",
		FromAgent: "agent-a",
		ToAgent:   "agent-b",
		Payload: &pb.Object{
			Id:   "payload-007",
			Type: "completely.unknown.type.v9",
			Data: []byte{0xDE, 0xAD, 0xBE, 0xEF},
		},
	}
	unknownTypeBytes, err := proto.Marshal(unknownType)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(unknownTypeBytes)

	f.Fuzz(func(t *testing.T, data []byte) {
		var pbHandoff pb.Handoff
		if err := proto.Unmarshal(data, &pbHandoff); err != nil {
			return // invalid protobuf, skip
		}
		// Must not panic — domain rejections are valid behavior
		protoToHandoff(&pbHandoff, domain.NoopTelemetryObserver{})
	})
}
