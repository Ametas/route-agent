// Package contract computes the proto contract hash reported by GetAgentInfo.
//
// It is the Go port of src/services/protoContract.service.ts and must stay bit-exact with the
// orchestrator's copy (orch:src/core/services/protoContractVerifier.ts:37-118, spec A §7.4): the
// orchestrator blocks every mutating RPC to a node whose hash differs.
//
// The TS agent re-read proto/agent.proto from cwd at runtime with proto-loader; here the schema
// is the FileDescriptor compiled into agentpb, so a wrong cwd can no longer empty the hash
// (spec A R-15). The canonical form, rebuilt from the descriptor:
//
//		{"messages":[{"fields":[{"label","name","number","type","type_name"?}…],"name":"agent.X"}…],
//		 "services":[{"methods":[{"name","request_stream","request_type","response_stream","response_type"}…],
//		              "name":"agent.EgressAgentService"}]}
//
//	  - field name: protobufjs camelCase (`_` + [a-z] → upper case letter, first char kept);
//	  - type/label: descriptor enum names (TYPE_STRING, LABEL_OPTIONAL/LABEL_REPEATED);
//	  - type_name: the BARE message name for message fields, absent otherwise;
//	  - fields by number; messages, services and methods by name (JS localeCompare — equal to
//	    byte order for every current name, spec A §7.4; the golden test catches a divergence);
//	  - object keys sorted, no whitespace, SHA-256, lower-case hex.
package contract

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/Ametas/route-agent/internal/agentpb"
)

// Source is the value of AgentInfoResponse.proto_contract_source.
const Source = "canonical-json"

// Expected is the hash of the current proto/agent.proto as computed by the orchestrator
// (spec A §7.4, verified 2026-11-14). Tests assert Hash() equals it.
const Expected = "83e5de4b90273573a467a295c07f71e80196840c91841dffe9de94185a8e840a"

var (
	once   sync.Once
	cached string
)

// Hash returns the contract hash of the compiled-in agent.proto, computed once.
func Hash() string {
	once.Do(func() { cached = HashOf(agentpb.File_agent_proto) })
	return cached
}

// HashOf hashes the canonical JSON of fd.
func HashOf(fd protoreflect.FileDescriptor) string {
	sum := sha256.Sum256(CanonicalJSON(fd))
	return hex.EncodeToString(sum[:])
}

// CanonicalJSON renders fd the way getCanonicalSchemaFromPackageDef + JSON.stringify does.
func CanonicalJSON(fd protoreflect.FileDescriptor) []byte {
	type field struct {
		number   int32
		name     string
		typ      string
		label    string
		typeName string
	}
	type message struct {
		name   string
		fields []field
	}
	type method struct {
		name, req, resp       string
		reqStream, respStream bool
	}
	type service struct {
		name    string
		methods []method
	}

	var messages []message
	var collect func(ms protoreflect.MessageDescriptors)
	collect = func(ms protoreflect.MessageDescriptors) {
		for i := 0; i < ms.Len(); i++ {
			md := ms.Get(i)
			m := message{name: string(md.FullName())}
			fs := md.Fields()
			for j := 0; j < fs.Len(); j++ {
				f := fs.Get(j)
				fl := field{
					number: int32(f.Number()),
					name:   camelCase(string(f.Name())),
					typ:    descriptorpb.FieldDescriptorProto_Type(f.Kind()).String(),
					label:  "LABEL_OPTIONAL",
				}
				if f.Cardinality() == protoreflect.Repeated {
					fl.label = "LABEL_REPEATED"
				}
				if f.Message() != nil {
					fl.typeName = string(f.Message().Name())
				}
				m.fields = append(m.fields, fl)
			}
			sort.SliceStable(m.fields, func(a, b int) bool { return m.fields[a].number < m.fields[b].number })
			messages = append(messages, m)
			collect(md.Messages()) // none today; kept so a nested type is not silently skipped
		}
	}
	collect(fd.Messages())

	var services []service
	for i := 0; i < fd.Services().Len(); i++ {
		sd := fd.Services().Get(i)
		s := service{name: string(sd.FullName())}
		for j := 0; j < sd.Methods().Len(); j++ {
			m := sd.Methods().Get(j)
			s.methods = append(s.methods, method{
				name:       string(m.Name()),
				req:        string(m.Input().Name()),
				resp:       string(m.Output().Name()),
				reqStream:  m.IsStreamingClient(),
				respStream: m.IsStreamingServer(),
			})
		}
		sort.SliceStable(s.methods, func(a, b int) bool { return s.methods[a].name < s.methods[b].name })
		if len(s.methods) > 0 {
			services = append(services, s)
		}
	}
	sort.SliceStable(messages, func(a, b int) bool { return messages[a].name < messages[b].name })
	sort.SliceStable(services, func(a, b int) bool { return services[a].name < services[b].name })

	var b bytes.Buffer
	str := func(s string) {
		var tmp bytes.Buffer
		enc := json.NewEncoder(&tmp)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(s)
		b.Write(bytes.TrimSuffix(tmp.Bytes(), []byte("\n")))
	}
	boolean := func(v bool) {
		if v {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	}
	b.WriteString(`{"messages":[`)
	for i, m := range messages {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"fields":[`)
		for j, f := range m.fields {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"label":`)
			str(f.label)
			b.WriteString(`,"name":`)
			str(f.name)
			b.WriteString(`,"number":`)
			b.WriteString(itoa(f.number))
			b.WriteString(`,"type":`)
			str(f.typ)
			if f.typeName != "" {
				b.WriteString(`,"type_name":`)
				str(f.typeName)
			}
			b.WriteByte('}')
		}
		b.WriteString(`],"name":`)
		str(m.name)
		b.WriteByte('}')
	}
	b.WriteString(`],"services":[`)
	for i, s := range services {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"methods":[`)
		for j, m := range s.methods {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"name":`)
			str(m.name)
			b.WriteString(`,"request_stream":`)
			boolean(m.reqStream)
			b.WriteString(`,"request_type":`)
			str(m.req)
			b.WriteString(`,"response_stream":`)
			boolean(m.respStream)
			b.WriteString(`,"response_type":`)
			str(m.resp)
			b.WriteByte('}')
		}
		b.WriteString(`],"name":`)
		str(s.name)
		b.WriteByte('}')
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// camelCase is protobufjs util.camelCase: keep the first character, then every "_x" with x in
// [a-z] becomes "X". "_1" and "_X" are kept as is.
func camelCase(s string) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.WriteByte(s[0])
	rest := s[1:]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '_' && i+1 < len(rest) && rest[i+1] >= 'a' && rest[i+1] <= 'z' {
			b.WriteByte(rest[i+1] - 'a' + 'A')
			i++
			continue
		}
		b.WriteByte(rest[i])
	}
	return b.String()
}

func itoa(n int32) string { return strconv.FormatInt(int64(n), 10) }
