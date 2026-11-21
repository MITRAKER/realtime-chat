package a

import "context"

func countdown(n int) int { // want `recursion: countdown -> countdown`
	if n == 0 {
		return 0
	}
	return countdown(n - 1)
}

func ping(n int) { // want `recursion: ping -> pong -> ping`
	if n > 0 {
		pong(n - 1)
	}
}

func pong(n int) { ping(n) }

// A longer cycle, whose call order differs from alphabetical order.
func alpha() { gamma() } // want `recursion: alpha -> gamma -> beta -> alpha`
func beta()  { alpha() }
func gamma() { beta() }

type tree struct{ kids []*tree }

func (t *tree) size() int { // want `recursion: tree.size -> tree.size`
	n := 1
	for _, k := range t.kids {
		n += k.size()
	}
	return n
}

// Calls through a closure count for the enclosing function.
func viaClosure(n int) { // want `recursion: viaClosure -> viaClosure`
	f := func() { viaClosure(n - 1) }
	f()
}

// Not recursion: calls a different function, and through an interface.
type sizer interface{ size() int }

func total(s sizer) int { return s.size() + helper() }
func helper() int       { return 1 }

func jumps() {
	i := 0
loop:
	i++
	if i < 3 {
		goto loop // want `goto is not allowed`
	}
}

func undocumented(ctx context.Context) {
	for { // want `loop without a condition can run forever`
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func documented(ctx context.Context) {
	// jpl:loop ends when ctx is cancelled.
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func emptyDirective(ctx context.Context) {
	// jpl:loop
	for { // want `loop without a condition can run forever`
		<-ctx.Done()
		return
	}
}

func channelRange(ch chan int) int {
	sum := 0
	for v := range ch { // want `range over a channel can run forever`
		sum += v
	}
	// jpl:loop ends when the sender closes ch.
	for v := range ch {
		sum += v
	}
	return sum
}

func bounded(xs []int) int {
	sum := 0
	for i := 0; i < len(xs); i++ {
		sum += xs[i]
	}
	for _, x := range xs {
		sum += x
	}
	for i := range 10 {
		sum += i
	}
	return sum
}
