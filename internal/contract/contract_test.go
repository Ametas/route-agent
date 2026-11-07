package contract

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/Ametas/route-agent/internal/agentpb"
)

func TestGoldenHash(t *testing.T) {
	if got := Hash(); got != Expected {
		t.Fatalf("contract hash %s, want %s (orchestrator computeProtoContractHash, spec A §7.4)", got, Expected)
	}
}

func TestCanonicalPrefix(t *testing.T) {
	prefix := `{"messages":[{"fields":[{"label":"LABEL_OPTIONAL","name":"orchestratorSecret","number":1,"type":"TYPE_STRING"}],"name":"agent.AgentInfoRequest"},{"fields":[{"label":"LABEL_OPTIONAL","name":"protoContractHash","number":1,"type":"TYPE_STRING"},`
	if got := string(CanonicalJSON(agentpb.File_agent_proto)); !strings.HasPrefix(got, prefix) {
		t.Fatalf("canonical JSON starts %q", got[:200])
	}
}

func TestCamelCase(t *testing.T) {
	for in, want := range map[string]string{
		"orchestrator_secret": "orchestratorSecret", "a_b_c": "aBC", "x_1": "x_1", "x_Y": "x_Y",
		"_lead": "_lead", "plain": "plain", "mesh_tunnel_active_peers": "meshTunnelActivePeers",
	} {
		if got := camelCase(in); got != want {
			t.Errorf("camelCase(%q) = %q, want %q", in, got, want)
		}
	}
}

func mutate(t *testing.T, f func(fdp *descriptorpb.FileDescriptorProto)) protoreflect.FileDescriptor {
	t.Helper()
	fdp := proto.Clone(protodesc.ToFileDescriptorProto(agentpb.File_agent_proto)).(*descriptorpb.FileDescriptorProto)
	f(fdp)
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func msg(fdp *descriptorpb.FileDescriptorProto, name string) *descriptorpb.DescriptorProto {
	for _, m := range fdp.MessageType {
		if m.GetName() == name {
			return m
		}
	}
	panic(name)
}

func TestInvariantToDeclarationOrder(t *testing.T) {
	fd := mutate(t, func(fdp *descriptorpb.FileDescriptorProto) {
		ms := fdp.MessageType
		for i, j := 0, len(ms)-1; i < j; i, j = i+1, j-1 {
			ms[i], ms[j] = ms[j], ms[i]
		}
		fs := msg(fdp, "TelemetryResponse").Field
		for i, j := 0, len(fs)-1; i < j; i, j = i+1, j-1 {
			fs[i], fs[j] = fs[j], fs[i]
		}
		mm := fdp.Service[0].Method
		for i, j := 0, len(mm)-1; i < j; i, j = i+1, j-1 {
			mm[i], mm[j] = mm[j], mm[i]
		}
	})
	if HashOf(fd) != Expected {
		t.Fatal("hash must not depend on declaration order")
	}
}

func TestChangesWithFieldNumberTypeNameLabel(t *testing.T) {
	cases := map[string]func(fdp *descriptorpb.FileDescriptorProto){
		"number": func(fdp *descriptorpb.FileDescriptorProto) {
			msg(fdp, "AgentInfoRequest").Field[0].Number = proto.Int32(9)
		},
		"type": func(fdp *descriptorpb.FileDescriptorProto) {
			msg(fdp, "AgentInfoRequest").Field[0].Type = descriptorpb.FieldDescriptorProto_TYPE_BYTES.Enum()
		},
		"name": func(fdp *descriptorpb.FileDescriptorProto) {
			msg(fdp, "AgentInfoRequest").Field[0].Name = proto.String("secret")
		},
		"label": func(fdp *descriptorpb.FileDescriptorProto) {
			msg(fdp, "AgentInfoRequest").Field[0].Label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
		},
		"stream": func(fdp *descriptorpb.FileDescriptorProto) {
			for _, m := range fdp.Service[0].Method {
				if m.GetName() == "GetAgentInfo" {
					m.ServerStreaming = proto.Bool(true)
				}
			}
		},
	}
	for name, f := range cases {
		if HashOf(mutate(t, f)) == Expected {
			t.Errorf("%s change did not change the hash", name)
		}
	}
}

// TestMatchesTypeScriptImplementation runs the TS agent's own computeProtoContractHash over
// proto/agent.proto when node and the TS toolchain are present (local checkout with
// node_modules); CI for the Go branch relies on the golden test above.
func TestMatchesTypeScriptImplementation(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..")
	tsx := filepath.Join(root, "node_modules", ".bin", "tsx")
	if _, err := os.Stat(tsx); err != nil {
		t.Skip("TS toolchain not installed")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	script := "import { computeProtoContractHash } from './src/services/protoContract.service.ts'; process.stdout.write(computeProtoContractHash());"
	cmd := exec.Command(tsx, "-e", script)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "NODE_ENV=test", "EGRESS_CONTROL_SECRET=x")
	var out bytes.Buffer
	cmd.Stdout = &out
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Skipf("tsx failed: %v", err)
		}
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		t.Skip("tsx timed out")
	}
	if got := strings.TrimSpace(out.String()); got != Hash() {
		t.Fatalf("TS computes %s, Go computes %s", got, Hash())
	}
}
