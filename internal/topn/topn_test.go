package topn

import "testing"

func TestCollector_KeepsBestAndBoundsMemory(t *testing.T) {
	c := New(5, func(a, b int) bool { return a > b })
	for i := 0; i < 100000; i++ {
		c.Add(i)
		if len(c.items) > 2*5+256 {
			t.Fatalf("collector held %d items, want bounded", len(c.items))
		}
	}
	got := c.Result()
	want := []int{99999, 99998, 99997, 99996, 99995}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	u := New(0, func(a, b int) bool { return a > b })
	for i := 0; i < 1000; i++ {
		u.Add(i)
	}
	if n := len(u.Result()); n != 1000 {
		t.Fatalf("unbounded kept %d", n)
	}
}
