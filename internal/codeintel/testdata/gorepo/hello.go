package demo

// Hello greets.
func Hello() string { return "hi" }

const Answer = 42

type Greeter struct{}

func (g Greeter) Greet() string { return Hello() }

func CallHello() string { return Hello() }
