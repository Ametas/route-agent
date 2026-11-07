package jsjson

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fixtures: testdata/v8_fixtures.json is produced by real V8 (Node 22) from the inputs listed in
// it (generator: JSON.stringify(JSON.parse(in), null, 2), JSON.stringify(JSON.parse(in)) and the
// SyntaxError message for the error cases). It is read with this package's own Parse so that
// lone surrogates survive.
func loadFixtures(t *testing.T) *Object {
	t.Helper()
	raw, err := os.ReadFile("testdata/v8_fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	v, err := Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*Object)
}

func field(o any, k string) string {
	v, _ := o.(*Object).Get(k)
	return v.(string)
}

func TestV8Fixtures(t *testing.T) {
	fx := loadFixtures(t)
	ok, _ := fx.Get("ok")
	for _, c := range ok.([]any) {
		in := field(c, "in")
		v, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if got, want := Stringify(v, "  "), field(c, "pretty"); got != want {
			t.Errorf("pretty(%q)\n got: %q\nwant: %q", in, got, want)
		}
		if got, want := Stringify(v, ""), field(c, "compact"); got != want {
			t.Errorf("compact(%q)\n got: %q\nwant: %q", in, got, want)
		}
	}
	errs, _ := fx.Get("err")
	for _, c := range errs.([]any) {
		in := field(c, "in")
		_, err := Parse(in)
		if err == nil {
			t.Errorf("Parse(%q) succeeded, want error", in)
			continue
		}
		if got, want := err.Error(), field(c, "msg"); got != want {
			t.Errorf("Parse(%q) error\n got: %q\nwant: %q", in, got, want)
		}
	}
}

func TestObjectOrder(t *testing.T) {
	o := NewObject()
	o.Set("b", 1.0)
	o.Set("3", 1.0)
	o.Set("a", 1.0)
	o.Set("1", 1.0)
	o.Set("b", 2.0) // re-set keeps position
	if got := strings.Join(o.Keys(), ","); got != "1,3,b,a" {
		t.Fatalf("keys = %s", got)
	}
	o.Delete("b")
	o.Set("b", 3.0) // delete + add moves to the end
	if got := strings.Join(o.Keys(), ","); got != "1,3,a,b" {
		t.Fatalf("keys after re-add = %s", got)
	}
	s := Spread([]any{"x", "y"})
	if got := Stringify(s, ""); got != `{"0":"x","1":"y"}` {
		t.Fatalf("spread array = %s", got)
	}
	if Spread(nil).Len() != 0 || Spread(5.0).Len() != 0 {
		t.Fatal("spread of a primitive must be empty")
	}
}

func TestFormatNumber(t *testing.T) {
	cases := map[float64]string{
		0: "0", math.Copysign(0, -1): "0", 1: "1", -1.5: "-1.5", 1e21: "1e+21", 1e20: "100000000000000000000",
		1e-6: "0.000001", 1e-7: "1e-7", 5e-324: "5e-324", 0.1: "0.1", 123.456: "123.456", 2.5e-10: "2.5e-10",
	}
	for f, want := range cases {
		if got := FormatNumber(f); got != want {
			t.Errorf("FormatNumber(%v) = %q, want %q", f, got, want)
		}
	}
}

func TestConversions(t *testing.T) {
	if ToUint32(-1) != 4294967295 || ToUint32(1.9) != 1 || ToUint32(math.NaN()) != 0 || ToUint32(4294967296+5) != 5 {
		t.Fatal("ToUint32")
	}
	if ToInt64(1.5) != 1 || ToInt64(-1.5) != -1 || ToInt64(5_000_000_000) != 5_000_000_000 || ToInt64(math.NaN()) != 0 {
		t.Fatal("ToInt64")
	}
	if ToNumber(" 443 ") != 443 || ToNumber("0x1BB") != 443 || ToNumber("1e3") != 1000 || ToNumber("") != 0 ||
		!math.IsNaN(ToNumber("abc")) || ToNumber(true) != 1 || ToNumber([]any{443.0}) != 443 || !math.IsNaN(ToNumber(NewObject())) {
		t.Fatal("ToNumber")
	}
}

// TestDifferentialAgainstNode feeds random JSON documents to the local node binary (when there is
// one) and compares both serializations and the error text. Skipped where node is absent (CI of
// the Go module does not need it; the committed fixtures cover the known edge cases).
func TestDifferentialAgainstNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	rng := rand.New(rand.NewSource(1))
	var inputs []string
	for i := 0; i < 300; i++ {
		inputs = append(inputs, randomJSON(rng, 0))
	}
	for i := 0; i < 100; i++ {
		s := randomJSON(rng, 0)
		if len(s) > 1 {
			cut := rng.Intn(len(s))
			if s[cut] >= 0x80 {
				continue // keep the input valid UTF-8: only ASCII bytes are mutated
			}
			s = s[:cut] + string("x,]}\"\\"[rng.Intn(6)]) + s[cut+1:]
		}
		inputs = append(inputs, s)
	}
	var arr []any
	for _, s := range inputs {
		arr = append(arr, s)
	}
	script := `const inputs = JSON.parse(require('fs').readFileSync(0, 'utf8'));
const out = inputs.map((s) => { try { const v = JSON.parse(s); return [JSON.stringify(v, null, 2), JSON.stringify(v), null]; } catch (e) { return [null, null, e.message]; } });
process.stdout.write(JSON.stringify(out));`
	cmd := exec.Command(node, "-e", script)
	cmd.Stdin = strings.NewReader(Stringify(arr, ""))
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("node: %v", err)
	}
	res, err := Parse(stdout.String())
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res.([]any) {
		row := r.([]any)
		v, perr := Parse(inputs[i])
		if row[2] != nil {
			if perr == nil || perr.Error() != row[2].(string) {
				t.Errorf("input %q: node error %q, go %v", inputs[i], row[2], perr)
			}
			continue
		}
		if perr != nil {
			t.Errorf("input %q: go error %v, node parsed it", inputs[i], perr)
			continue
		}
		if got := Stringify(v, "  "); got != row[0].(string) {
			t.Errorf("input %q pretty:\n go: %q\nnode: %q", inputs[i], got, row[0])
		}
		if got := Stringify(v, ""); got != row[1].(string) {
			t.Errorf("input %q compact:\n go: %q\nnode: %q", inputs[i], got, row[1])
		}
	}
}

func randomJSON(rng *rand.Rand, depth int) string {
	k := rng.Intn(8)
	if depth > 3 {
		k = rng.Intn(4)
	}
	switch k {
	case 0:
		return []string{"null", "true", "false"}[rng.Intn(3)]
	case 1:
		nums := []string{"0", "-0", "1", "1e21", "1.5e-7", "123456789012345678", "0.1", "-3.25", "1E+2", "5e-324", "1e309"}
		if rng.Intn(2) == 0 {
			return nums[rng.Intn(len(nums))]
		}
		return fmt.Sprintf("%g", rng.NormFloat64()*math.Pow(10, float64(rng.Intn(40)-20)))
	case 2, 3:
		return randomString(rng)
	case 4, 5:
		n := rng.Intn(4)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = randomJSON(rng, depth+1)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		n := rng.Intn(5)
		parts := make([]string, n)
		for i := range parts {
			parts[i] = randomString(rng) + ":" + randomJSON(rng, depth+1)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
}

func randomString(rng *rand.Rand) string {
	pieces := []string{"a", "tag", "1", "0", "10", "007", "4294967295", "é", "日", "😀", `\n`, `\u0001`, `\ud800`, `\udc00`, `😀`, `\"`, `\\`, `\/`, "<", "&", " ", " ", "-1"}
	n := rng.Intn(4)
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < n; i++ {
		b.WriteString(pieces[rng.Intn(len(pieces))])
	}
	b.WriteByte('"')
	return b.String()
}
