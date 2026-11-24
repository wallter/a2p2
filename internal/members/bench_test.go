package members

import (
	"fmt"
	"testing"
)

// chainOf builds a room whose creator adds n-1 machines, each entry naming
// the one before it: the worst shape for "what each entry had seen".
func chainOf(n int) []entry {
	es := []entry{newEntry(entry{Author: "A", Seq: 1, Kind: kCreate})}
	for i := 1; i < n; i++ {
		p := es[i-1]
		es = append(es, newEntry(entry{Author: "A", Seq: int64(i + 1), Prev: p.ID, Parents: []string{p.ID}, Kind: kAdd, Target: fmt.Sprintf("M%d", i)}))
	}
	return es
}

func benchResolve(b *testing.B, n int) {
	es := chainOf(n)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		final.resolveRoom(es[0].ID, es)
	}
}

func BenchmarkResolveChain1000(b *testing.B) { benchResolve(b, 1000) }
func BenchmarkResolveChain3000(b *testing.B) { benchResolve(b, 3000) }

// One more entry into a machine already holding 3,000 (each iteration
// receives the next entry of the chain).
func BenchmarkReceiveOneIntoMachineOf3000(b *testing.B) {
	es := chainOf(3000 + b.N)
	m := newMachine(final, es[0])
	m.receive(es[1:3000]...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if res := m.receive(es[3000+i]); len(res) != 1 || res[0].Disposition != Accepted {
			b.Fatal(res)
		}
	}
}
