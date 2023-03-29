package vm

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/cloudcmds/tamarin/internal/compiler"
	"github.com/cloudcmds/tamarin/internal/op"
	"github.com/cloudcmds/tamarin/object"
	"github.com/cloudcmds/tamarin/parser"
)

const (
	// MaxFrameCount = 2048
	MaxArgs = 255
)

func Run(code string) (object.Object, error) {
	ast, err := parser.Parse(code)
	if err != nil {
		return nil, err
	}
	c := compiler.New(compiler.Options{
		GlobalSymbols: compiler.NewGlobalSymbols(),
		Name:          "main",
	})
	mainScope, err := c.Compile(ast)
	if err != nil {
		return nil, err
	}
	vm := New(mainScope)
	if err := vm.Run(); err != nil {
		return nil, err
	}
	return vm.Pop(), nil
}

const MaxFrameDepth = 1024

type VM struct {
	ip    int
	sp    int
	stack *Stack[object.Object]
	// frameStack   *Stack[*Frame]
	frames       [MaxFrameDepth]Frame
	framesIndex  int
	main         *compiler.Scope
	currentScope *compiler.Scope
	globals      []object.Object
	// framePool    sync.Pool
	// arrayPool sync.Pool
}

func New(main *compiler.Scope) *VM {
	vm := &VM{
		stack: NewStack[object.Object](1024),
		// frameStack:   NewStack[*Frame](1024),
		sp:           -1,
		main:         main,
		currentScope: main,
		// framePool: sync.Pool{
		// 	New: func() interface{} {
		// 		return &Frame{}
		// 	},
		// },
		// arrayPool: sync.Pool{
		// 	New: func() interface{} {
		// 		slice := make([]object.Object, 8)
		// 		return &slice
		// 	},
		// },
	}
	if main.Symbols != nil {
		m := main.Symbols.Map()
		vm.globals = make([]object.Object, len(m))
		for _, sym := range m {
			if sym.Attrs.Value != nil {
				vm.globals[sym.Index] = sym.Attrs.Value.(object.Object)
			}
		}
	}
	return vm
}

func (vm *VM) Run() error {
	// for i, b := range vm.code {
	// 	fmt.Printf("%d %d\n", i, b)
	// }
	// fmt.Println("---")
	ctx := context.Background()
	// currentFrame := NewFrame(nil, make([]object.Object, symbolCount), 0, vm.currentScope)
	tmpArgs := [MaxArgs]object.Object{}
	currentFrame := &vm.frames[0]
	currentFrame.Init(nil, 0, vm.currentScope.Symbols.Size())
	currentFrame.scope = vm.main
	// vm.frameStack.Push(currentFrame)
	for vm.ip < len(vm.currentScope.Instructions) {
		scope := vm.currentScope
		opcode := scope.Instructions[vm.ip]
		// opinfo := op.GetInfo(opcode)
		// _, operands := compiler.ReadOp(scope.Instructions[vm.ip:])
		// fmt.Printf("EXEC %-25s %v (IP: %d)\n", opinfo.Name, operands, vm.ip)
		vm.ip++
		switch opcode {
		case op.Nop:
		case op.LoadAttr:
			obj := vm.Pop()
			name := vm.currentScope.Names[vm.fetch2()]
			value, found := obj.GetAttr(name)
			if !found {
				return fmt.Errorf("attribute %q not found", name)
			}
			vm.stack.Push(value)
		case op.LoadConst:
			vm.stack.Push(scope.Constants[vm.fetch2()])
		case op.LoadFast:
			vm.stack.Push(currentFrame.locals[vm.fetch2()])
		case op.LoadGlobal:
			vm.stack.Push(vm.globals[vm.fetch2()])
		case op.LoadFree:
			freeVars := currentFrame.fn.FreeVars()
			vm.stack.Push(freeVars[vm.fetch2()].Value())
		case op.StoreFast:
			currentFrame.locals[vm.fetch()] = vm.Pop()
		case op.StoreGlobal:
			vm.globals[vm.fetch2()] = vm.Pop()
		case op.StoreFree:
			freeVars := currentFrame.fn.FreeVars()
			freeVars[vm.fetch2()].Set(vm.Pop())
		case op.LoadClosure:
			constIndex := vm.fetch2()
			freeCount := vm.fetch2()
			free := make([]*object.Cell, freeCount)
			for i := uint16(0); i < freeCount; i++ {
				obj := vm.Pop()
				switch obj := obj.(type) {
				case *object.Cell:
					free[i] = obj
				default:
					return errors.New("expected cell")
				}
			}
			fn := scope.Constants[constIndex].(*object.CompiledFunction)
			closure := object.NewClosure(fn, fn.Scope(), free)
			vm.stack.Push(closure)
		case op.MakeCell:
			symbolIndex := vm.fetch2()
			framesBack := int(vm.fetch())
			frameIndex := vm.framesIndex - framesBack
			if frameIndex < 0 {
				return fmt.Errorf("no frame at depth %d", framesBack)
			}
			frame := &vm.frames[frameIndex]
			locals := frame.Locals()
			vm.stack.Push(object.NewCell(&locals[symbolIndex]))
		case op.Nil:
			vm.stack.Push(object.Nil)
		case op.True:
			vm.stack.Push(object.True)
		case op.False:
			vm.stack.Push(object.False)
		case op.CompareOp:
			opType := op.CompareOpType(vm.fetch())
			b := vm.Pop()
			a := vm.Pop()
			vm.stack.Push(vm.runCompareOp(opType, a, b))
		case op.BinaryOp:
			opType := op.BinaryOpType(vm.fetch())
			b := vm.Pop()
			a := vm.Pop()
			vm.stack.Push(vm.runBinaryOp(opType, a, b))
		case op.Call:
			argc := int(vm.fetch())
			for i := 0; i < argc; i++ {
				tmpArgs[argc-1-i] = vm.Pop()
			}
			obj := vm.Pop()
			switch obj := obj.(type) {
			case *object.Builtin:
				result := obj.Call(ctx, tmpArgs[:argc]...)
				vm.stack.Push(result)
			case *object.CompiledFunction:
				if vm.framesIndex+1 >= MaxFrameDepth {
					fmt.Println("OVERFLOW", vm.framesIndex)
					return errors.New("frame overflow")
				}
				vm.framesIndex++
				frame := &vm.frames[vm.framesIndex]
				scope := obj.Scope().(*compiler.Scope)
				if scope.IsNamed {
					tmpArgs[argc] = obj
					argc++
				}
				// fmt.Println("frame.InitWithLocals", argc, vm.ip, tmpArgs[:argc])
				frame.InitWithLocals(obj, vm.ip, tmpArgs[:argc])
				// frame := currentFrame.NewChild(obj, locals, vm.ip)
				// frame := vm.framePool.Get().(*Frame)
				// frame.fn = obj
				// frame.locals = args
				// frame.returnAddr = vm.ip
				// frame.parent = currentFrame
				// frame.scope = scope
				// vm.frameStack.Push(frame)
				currentFrame = frame
				vm.ip = 0
				vm.currentScope = scope
			default:
				return fmt.Errorf("not a function: %T", obj)
			}
		case op.ReturnValue:
			if vm.framesIndex < 1 {
				return errors.New("frame underflow")
			}
			leaving := &vm.frames[vm.framesIndex]
			vm.framesIndex--
			currentFrame = &vm.frames[vm.framesIndex]
			vm.ip = leaving.returnAddr
			vm.currentScope = currentFrame.Scope()
			// currentFrame = frame.parent
			// vm.framePool.Put(frame)
			// vm.arrayPool.Put(&frame.locals)
		case op.PopJumpForwardIfTrue:
			tos := vm.Pop()
			delta := int(vm.fetch2()) - 3
			if tos.IsTruthy() {
				vm.ip += delta
			}
		case op.PopJumpForwardIfFalse:
			tos := vm.Pop()
			delta := int(vm.fetch2()) - 3
			if !tos.IsTruthy() {
				vm.ip += delta
			}
		case op.PopJumpBackwardIfTrue:
			tos := vm.Pop()
			delta := int(vm.fetch2()) - 3
			if tos.IsTruthy() {
				vm.ip -= delta
			}
		case op.PopJumpBackwardIfFalse:
			tos := vm.Pop()
			delta := int(vm.fetch2()) - 3
			if !tos.IsTruthy() {
				vm.ip -= delta
			}
		case op.JumpForward:
			base := vm.ip - 1
			delta := int(vm.fetch2())
			vm.ip = base + delta
		case op.JumpBackward:
			base := vm.ip - 1
			delta := int(vm.fetch2())
			vm.ip = base - delta
		case op.Print:
			fmt.Println("PRINT", vm.top())
		case op.BuildList:
			count := vm.fetch2()
			items := make([]object.Object, count)
			for i := uint16(0); i < count; i++ {
				items[count-1-i] = vm.Pop()
			}
			vm.stack.Push(object.NewList(items))
		case op.BuildMap:
			count := vm.fetch2()
			items := make(map[string]object.Object, count)
			for i := uint16(0); i < count; i++ {
				v := vm.Pop()
				k := vm.Pop()
				items[k.(*object.String).Value()] = v
			}
			vm.stack.Push(object.NewMap(items))
		case op.BuildSet:
			count := vm.fetch2()
			items := make([]object.Object, count)
			for i := uint16(0); i < count; i++ {
				items[i] = vm.Pop()
			}
			vm.stack.Push(object.NewSet(items))
		case op.BinarySubscr:
			index := vm.Pop()
			obj := vm.Pop()
			container, ok := obj.(object.Container)
			if !ok {
				return fmt.Errorf("object is not a container: %T", obj)
			}
			result, err := container.GetItem(index)
			if err != nil {
				return err.Value()
			}
			vm.stack.Push(result)
		case op.UnaryNegative:
			obj := vm.Pop()
			switch obj := obj.(type) {
			case *object.Int:
				vm.stack.Push(object.NewInt(-obj.Value()))
			case *object.Float:
				vm.stack.Push(object.NewFloat(-obj.Value()))
			default:
				return fmt.Errorf("object is not a number: %T", obj)
			}
		case op.UnaryNot:
			obj := vm.Pop()
			if obj.IsTruthy() {
				vm.stack.Push(object.False)
			} else {
				vm.stack.Push(object.True)
			}
		case op.ContainsOp:
			obj := vm.Pop()
			containerObj := vm.Pop()
			invert := vm.fetch() == 1
			if container, ok := containerObj.(object.Container); ok {
				value := container.Contains(obj)
				if invert {
					value = object.Not(value)
				}
				vm.stack.Push(value)
			} else {
				return fmt.Errorf("object is not a container: %T", container)
			}
		case op.Halt:
			return nil
		default:
			return fmt.Errorf("unknown opcode: %d", opcode)
		}
	}
	return nil
}

func (vm *VM) runCompareOp(opType op.CompareOpType, a, b object.Object) object.Object {
	switch opType {
	case op.Equal:
		return a.Equals(b)
	case op.NotEqual:
		if a.Equals(b) == object.True {
			return object.False
		} else {
			return object.True
		}
	case op.LessThan:
		return object.NewBool(a.(*object.Int).Value() < b.(*object.Int).Value())
	case op.LessThanOrEqual:
		return object.NewBool(a.(*object.Int).Value() <= b.(*object.Int).Value())
	case op.GreaterThan:
		return object.NewBool(a.(*object.Int).Value() > b.(*object.Int).Value())
	case op.GreaterThanOrEqual:
		return object.NewBool(a.(*object.Int).Value() >= b.(*object.Int).Value())
	default:
		panic("unknown compare op")
	}
}

func (vm *VM) runBinaryOp(opType op.BinaryOpType, a, b object.Object) object.Object {
	switch opType {
	case op.Add:
		return object.NewInt(a.(*object.Int).Value() + b.(*object.Int).Value())
	case op.Subtract:
		return object.NewInt(a.(*object.Int).Value() - b.(*object.Int).Value())
	case op.Multiply:
		return object.NewInt(a.(*object.Int).Value() * b.(*object.Int).Value())
	case op.Divide:
		return object.NewInt(a.(*object.Int).Value() / b.(*object.Int).Value())
	case op.Modulo:
		return object.NewInt(a.(*object.Int).Value() % b.(*object.Int).Value())
	case op.And:
		return object.NewInt(a.(*object.Int).Value() & b.(*object.Int).Value())
	case op.Or:
		return object.NewInt(a.(*object.Int).Value() | b.(*object.Int).Value())
	case op.Xor:
		return object.NewInt(a.(*object.Int).Value() ^ b.(*object.Int).Value())
	case op.Power:
		return object.NewInt(int64(math.Pow(float64(a.(*object.Int).Value()), float64(b.(*object.Int).Value()))))
	case op.LShift:
		return object.NewInt(a.(*object.Int).Value() << b.(*object.Int).Value())
	case op.RShift:
		return object.NewInt(a.(*object.Int).Value() >> b.(*object.Int).Value())
	}
	return nil
}

func (vm *VM) TOS() (object.Object, bool) {
	return vm.stack.Top()
}

// func (vm *VM) Frame() (*Frame, bool) {
// 	return vm.frameStack.Top()
// }

func (vm *VM) Pop() object.Object {
	obj, _ := vm.stack.Pop()
	return obj
}

func (vm *VM) top() object.Object {
	obj, ok := vm.stack.Top()
	if !ok {
		return nil
	}
	return obj
}

func (vm *VM) fetch() uint8 {
	ip := vm.ip
	vm.ip++
	return uint8(vm.currentScope.Instructions[ip])
}

func (vm *VM) fetch2() uint16 {
	instr := vm.currentScope.Instructions
	value := uint16(instr[vm.ip]) | uint16(instr[vm.ip+1])<<8
	vm.ip += 2
	return value
}
