package locator

import "testing"

func TestResolutionCache_HitMissEvict(t *testing.T) {
	c, err := newResolutionCache(2)
	if err != nil {
		t.Fatalf("newResolutionCache: %v", err)
	}

	a := podKey{Namespace: "ns", Name: "a"}
	b := podKey{Namespace: "ns", Name: "b"}
	x := podKey{Namespace: "ns", Name: "x"}

	c.add(a, chainNode{Namespace: "ns", Kind: "Deployment", Name: "da"})
	c.add(b, chainNode{Namespace: "ns", Kind: "Deployment", Name: "db"})

	if got, hit := c.get(a); !hit || got.Name != "da" {
		t.Fatalf("a: hit=%v got=%v", hit, got)
	}
	// Adding x evicts the least-recently-used entry; a was just used so b should evict.
	c.add(x, chainNode{Namespace: "ns", Kind: "Deployment", Name: "dx"})
	if _, hit := c.get(b); hit {
		t.Errorf("b should have been evicted")
	}
	if _, hit := c.get(a); !hit {
		t.Errorf("a should still be present")
	}
}

func TestResolutionCache_NegativeEntry(t *testing.T) {
	c, err := newResolutionCache(8)
	if err != nil {
		t.Fatalf("newResolutionCache: %v", err)
	}
	k := podKey{Namespace: "ns", Name: "p"}
	c.add(k, chainNode{}) // negative entry (zero value)

	got, hit := c.get(k)
	if !hit {
		t.Fatalf("expected hit")
	}
	if got != (chainNode{}) {
		t.Errorf("got=%v, want zero chainNode", got)
	}
}
