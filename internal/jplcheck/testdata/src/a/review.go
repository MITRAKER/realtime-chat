package a

// Cases a review found that the first version of jplcheck missed.

import "context"

// Rule 1: calls the first version didn't follow.

// A method of a generic type, called through another instantiation.
type gtree[T any] struct{ kids []*gtree[T] }

func (t *gtree[T]) size() int { return gsize(t) }

func gsize[T any](t *gtree[T]) int { // want `recursion: gsize -> gtree.size -> gsize`
	n := 1
	for _, k := range t.kids {
		n += k.size()
	}
	return n
}

type box[T any] struct{ v T }

func (b box[T]) get() T { // want `recursion: box.get -> useBox -> box.get`
	useBox()
	return b.v
}

func useBox() { box[int]{}.get() }

// Method expressions: T.m(t, x) and (*T).m(t, x).
type counter struct{}

func (c counter) down(n int) { // want `recursion: counter.down -> counter.down`
	if n > 0 {
		counter.down(c, n-1)
	}
}

func (c *counter) up(n int) { // want `recursion: counter.up -> counter.up`
	if n < 10 {
		(*counter).up(c, n+1)
	}
}

// Explicit instantiation with two type arguments.
func pair[A, B any](a A, b B, n int) { // want `recursion: pair -> pair`
	if n > 0 {
		pair[A, B](a, b, n-1)
	}
}

// A method declared on an alias is shown with the real type's name.
type impl struct{}

type alias = impl

func (alias) run(n int) { // want `recursion: impl.run -> run -> impl.run`
	if n > 0 {
		run(n - 1)
	}
}

func run(n int) {
	var a alias
	a.run(n)
}

// Rule 2: loops the first version didn't flag, or flagged wrongly.

func labeled(ctx context.Context) {
	// jpl:loop ends when ctx is cancelled.
outer:
	for {
		select {
		case <-ctx.Done():
			break outer
		default:
		}
	}
}

func labeledUndocumented(ctx context.Context) {
outer:
	for { // want `loop without a condition can run forever`
		select {
		case <-ctx.Done():
			break outer
		default:
		}
	}
}

func emptyBlockDirective(ctx context.Context) {
	/* jpl:loop */
	for { // want `loop without a condition can run forever`
		<-ctx.Done()
		return
	}
}

func blockDirective(ctx context.Context) {
	/* jpl:loop ends when ctx is cancelled. */
	for {
		<-ctx.Done()
		return
	}
}

func alwaysTrue() int {
	n := 0
	for i := 0; true; i++ { // want `loop whose condition is always true can run forever`
		n += i
		if n > 100 {
			break
		}
	}
	return n
}

func naturals(yield func(int) bool) {
	for i := 0; yield(i); i++ {
	}
}

func iterate() int {
	sum := 0
	for v := range naturals { // want `range over a function iterator can run forever`
		if sum += v; sum > 100 {
			break
		}
	}
	return sum
}

func genericChannel[C ~chan int | ~<-chan int](c C) int {
	sum := 0
	for v := range c { // want `range over a channel can run forever`
		sum += v
	}
	return sum
}

type receiveOnly interface{ ~<-chan string }

func genericNamedConstraint[C receiveOnly](c C) int {
	n := 0
	for range c { // want `range over a channel can run forever`
		n++
	}
	return n
}

func genericSlice[S ~[]int](s S) int {
	sum := 0
	for _, v := range s {
		sum += v
	}
	return sum
}
