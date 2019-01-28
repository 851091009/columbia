// Copyright 2017 The go-interpreter Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package exec provides functions for executing WebAssembly bytecode.
package exec

import (
	gcontext "context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/evanphx/columbia/exec/internal/compile"
	"github.com/go-interpreter/wagon/disasm"
	"github.com/go-interpreter/wagon/wasm"
	"github.com/go-interpreter/wagon/wasm/operators"
	ops "github.com/go-interpreter/wagon/wasm/operators"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/pkg/errors"
)

var (
	// ErrMultipleLinearMemories is returned by (*VM).NewVM when the module
	// has more then one entries in the linear memory space.
	ErrMultipleLinearMemories = errors.New("exec: more than one linear memories in module")
	// ErrInvalidArgumentCount is returned by (*VM).ExecCode when an invalid
	// number of arguments to the WebAssembly function are passed to it.
	ErrInvalidArgumentCount = errors.New("exec: invalid number of arguments to function")
)

// InvalidReturnTypeError is returned by (*VM).ExecCode when the module
// specifies an invalid return type value for the executed function.
type InvalidReturnTypeError int8

func (e InvalidReturnTypeError) Error() string {
	return fmt.Sprintf("Function has invalid return value_type: %d", int8(e))
}

// InvalidFunctionIndexError is returned by (*VM).ExecCode when the function
// index provided is invalid.
type InvalidFunctionIndexError int64

func (e InvalidFunctionIndexError) Error() string {
	return fmt.Sprintf("Invalid index to function index space: %d", int64(e))
}

type context struct {
	stack   []uint64
	locals  []uint64
	code    []byte
	pc      int64
	curFunc int64
}

type frame struct {
	fp   int64
	sp   int64
	ip   int64
	code []byte
	fn   *compiledFunction
}

// VM is the execution context for executing WebAssembly bytecode.
type VM struct {
	gctx gcontext.Context
	// ctx  context

	frames   []frame
	frameIdx int
	frame    *frame
	stack    []uint64

	module  *wasm.Module
	globals []uint64
	memory  Memory
	funcs   []function

	funcTable [256]func()

	// RecoverPanic controls whether the `ExecCode` method
	// recovers from a panic and returns it as an error
	// instead.
	// A panic can occur either when executing an invalid VM
	// or encountering an invalid instruction, e.g. `unreachable`.
	RecoverPanic bool

	abort bool // Flag for host functions to terminate execution
}

// As per the WebAssembly spec: https://github.com/WebAssembly/design/blob/27ac254c854994103c24834a994be16f74f54186/Semantics.md#linear-memory
const wasmPageSize = 65536 // (64 KB)

var endianess = binary.LittleEndian

// NewVM creates a new VM from a given module. If the module defines a
// start function, it will be executed.
func NewVM(ctx gcontext.Context, module *wasm.Module, memory Memory) (*VM, error) {
	var vm VM

	if module.Memory != nil && len(module.Memory.Entries) != 0 {
		if len(module.Memory.Entries) > 1 {
			return nil, ErrMultipleLinearMemories
		}

		sz := int32(module.Memory.Entries[0].Limits.Initial) * wasmPageSize

		if memory == nil {
			memory = NewSliceMemory(make([]byte, sz))
			hclog.L().Info("initalize slice memory", "size", sz)
		}

		slice, err := memory.Project(0, sz)
		if err != nil {
			return nil, errors.Wrap(err, "attempting to load initial linear memory")
		}

		copy(slice, module.LinearMemoryIndexSpace[0])
		vm.memory = memory
	}

	vm.funcs = make([]function, len(module.FunctionIndexSpace))
	vm.globals = make([]uint64, len(module.GlobalIndexSpace))
	vm.newFuncTable()
	vm.module = module

	nNatives := 0
	for i, fn := range module.FunctionIndexSpace {
		// Skip native methods as they need not be
		// disassembled; simply add them at the end
		// of the `funcs` array as is, as specified
		// in the spec. See the "host functions"
		// section of:
		// https://webassembly.github.io/spec/core/exec/modules.html#allocation
		if fn.IsHost() {
			vm.funcs[i] = goFunction{
				typ: fn.Host.Type(),
				val: fn.Host,
			}
			nNatives++
			continue
		}

		disassembly, err := disasm.NewDisassembly(fn, module)
		if err != nil {
			return nil, err
		}

		totalLocalVars := 0
		totalLocalVars += len(fn.Sig.ParamTypes)
		for _, entry := range fn.Body.Locals {
			totalLocalVars += int(entry.Count)
		}
		code, table := compile.Compile(disassembly.Code)
		vm.funcs[i] = compiledFunction{
			code:           code,
			branchTables:   table,
			maxDepth:       disassembly.MaxDepth,
			totalLocalVars: totalLocalVars,
			args:           len(fn.Sig.ParamTypes),
			returns:        len(fn.Sig.ReturnTypes) != 0,
		}
	}

	for i, global := range module.GlobalIndexSpace {
		val, err := module.ExecInitExpr(global.Init)
		if err != nil {
			return nil, err
		}
		switch v := val.(type) {
		case int32:
			vm.globals[i] = uint64(v)
		case int64:
			vm.globals[i] = uint64(v)
		case float32:
			vm.globals[i] = uint64(math.Float32bits(v))
		case float64:
			vm.globals[i] = uint64(math.Float64bits(v))
		}
	}

	if module.Start != nil {
		_, err := vm.ExecCode(int64(module.Start.Index))
		if err != nil {
			return nil, err
		}
	}

	vm.gctx = setVM(ctx, &vm)

	return &vm, nil
}

// Memory returns the linear memory space for the VM.
func (vm *VM) Memory() Memory {
	return vm.memory
}

func (vm *VM) pushBool(v bool) {
	if v {
		vm.pushUint64(1)
	} else {
		vm.pushUint64(0)
	}
}

func (vm *VM) fetchBool() bool {
	return vm.fetchInt8() != 0
}

func (vm *VM) fetchInt8() int8 {
	i := int8(vm.frame.code[vm.frame.ip])
	vm.frame.ip++
	return i
}

func (vm *VM) fetchUint32() uint32 {
	v := endianess.Uint32(vm.frame.code[vm.frame.ip:])
	vm.frame.ip += 4
	return v
}

func (vm *VM) fetchInt32() int32 {
	return int32(vm.fetchUint32())
}

func (vm *VM) fetchFloat32() float32 {
	return math.Float32frombits(vm.fetchUint32())
}

func (vm *VM) fetchUint64() uint64 {
	v := endianess.Uint64(vm.frame.code[vm.frame.ip:])
	vm.frame.ip += 8
	return v
}

func (vm *VM) fetchInt64() int64 {
	return int64(vm.fetchUint64())
}

func (vm *VM) fetchFloat64() float64 {
	return math.Float64frombits(vm.fetchUint64())
}

func (vm *VM) popUint64() uint64 {
	i := vm.stack[vm.frame.sp]
	vm.frame.sp--
	return i
}

func (vm *VM) popInt64() int64 {
	return int64(vm.popUint64())
}

func (vm *VM) popFloat64() float64 {
	return math.Float64frombits(vm.popUint64())
}

func (vm *VM) popUint32() uint32 {
	return uint32(vm.popUint64())
}

func (vm *VM) popInt32() int32 {
	return int32(vm.popUint32())
}

func (vm *VM) popFloat32() float32 {
	return math.Float32frombits(vm.popUint32())
}

func (vm *VM) pushUint64(i uint64) {
	vm.frame.sp++
	vm.stack[vm.frame.sp] = i

	// Debugf("|>                                 set_stack  sp=%d val=%d\n", vm.frame.sp, i)
}

func (vm *VM) pushInt64(i int64) {
	vm.pushUint64(uint64(i))
}

func (vm *VM) pushFloat64(f float64) {
	vm.pushUint64(math.Float64bits(f))
}

func (vm *VM) pushUint32(i uint32) {
	vm.pushUint64(uint64(i))
}

func (vm *VM) pushInt32(i int32) {
	vm.pushUint64(uint64(i))
}

func (vm *VM) pushFloat32(f float32) {
	vm.pushUint32(math.Float32bits(f))
}

// ExecCode calls the function with the given index and arguments.
// fnIndex should be a valid index into the function index space of
// the VM's module.
func (vm *VM) ExecCode(fnIndex int64, args ...uint64) (rtrn interface{}, err error) {
	// If used as a library, client code should set vm.RecoverPanic to true
	// in order to have an error returned.
	if vm.RecoverPanic {
		defer func() {
			if r := recover(); r != nil {
				switch e := r.(type) {
				case error:
					err = e
				default:
					err = fmt.Errorf("exec: %v", e)
				}
			}
		}()
	}

	if int(fnIndex) > len(vm.funcs) {
		return nil, InvalidFunctionIndexError(fnIndex)
	}

	if len(vm.module.GetFunction(int(fnIndex)).Sig.ParamTypes) != len(args) {
		return nil, ErrInvalidArgumentCount
	}

	compiled, ok := vm.funcs[fnIndex].(compiledFunction)
	if !ok {
		panic(fmt.Sprintf("exec: function at index %d is not a compiled function", fnIndex))
	}

	initStack := compiled.maxDepth + compiled.totalLocalVars + 1024

	vm.stack = make([]uint64, initStack)
	vm.frames = make([]frame, 100)

	vm.frame = &vm.frames[0]
	vm.frame.code = compiled.code
	vm.frame.fn = &compiled

	for i, arg := range args {
		vm.stack[i] = arg
	}

	vm.frame.sp = int64(len(args) + compiled.totalLocalVars - 1)

	hclog.L().Info("start", "sp", vm.frame.sp, "fp", vm.frame.fp, "args", len(args), "locals", compiled.totalLocalVars)

	res := vm.execCode()
	if compiled.returns {
		rtrnType := vm.module.GetFunction(int(fnIndex)).Sig.ReturnTypes[0]
		switch rtrnType {
		case wasm.ValueTypeI32:
			rtrn = uint32(res)
		case wasm.ValueTypeI64:
			rtrn = uint64(res)
		case wasm.ValueTypeF32:
			rtrn = math.Float32frombits(uint32(res))
		case wasm.ValueTypeF64:
			rtrn = math.Float64frombits(res)
		default:
			return nil, InvalidReturnTypeError(rtrnType)
		}
	}

	return rtrn, nil
}

func (vm *VM) execCode() uint64 {
	for {
	instloop:
		for int(vm.frame.ip) < len(vm.frame.code) && !vm.abort {
			op := vm.frame.code[vm.frame.ip]
			desc, err := operators.New(op)
			if err != nil {
				panic(err)
			}

			Debugf("% 3d %10s sp: % 2d %+v\n", vm.frame.ip, desc.Name, vm.frame.sp,
				vm.stack[vm.frame.fp:vm.frame.sp+1])
			vm.frame.ip++
			switch op {
			case ops.Return:
				break instloop
			case compile.OpJmp:
				vm.frame.ip = vm.fetchInt64()
				continue
			case compile.OpJmpZ:
				target := vm.fetchInt64()
				if vm.popUint32() == 0 {
					vm.frame.ip = target
					continue
				}
			case compile.OpJmpNz:
				target := vm.fetchInt64()
				preserveTop := vm.fetchBool()
				discard := vm.fetchInt64()
				if vm.popUint32() != 0 {
					vm.frame.ip = target
					var top uint64
					if preserveTop {
						top = vm.stack[vm.frame.sp]
					}
					vm.frame.sp -= int64(discard)
					if preserveTop {
						vm.pushUint64(top)
					}
					continue
				}
			case ops.BrTable:
				index := vm.fetchInt64()
				label := vm.popInt32()
				cf := vm.frame.fn
				table := cf.branchTables[index]
				var target compile.Target
				if label >= 0 && label < int32(len(table.Targets)) {
					target = table.Targets[int32(label)]
				} else {
					target = table.DefaultTarget
				}

				if target.Return {
					break instloop
				}

				vm.frame.ip = int64(target.Addr)
				var top uint64
				if target.PreserveTop {
					top = vm.stack[vm.frame.sp]
				}
				vm.frame.sp -= int64(target.Discard)
				if target.PreserveTop {
					vm.pushUint64(top)
				}
				continue
			case compile.OpDiscard:
				place := vm.fetchInt64()
				vm.frame.sp -= place
			case compile.OpDiscardPreserveTop:
				top := vm.stack[vm.frame.sp]
				place := vm.fetchInt64()
				vm.frame.sp -= place
				vm.pushUint64(top)
			default:
				vm.funcTable[op]()
			}
		}

		var (
			top     = vm.stack[vm.frame.sp]
			returns = vm.frame.fn.returns
		)

		if vm.frameIdx == 0 {
			if returns {
				return top
			} else {
				return 0
			}
		}

		vm.frameIdx--
		vm.frame = &vm.frames[vm.frameIdx]

		Debugf("|> ret frame=%d\n", vm.frameIdx)

		if returns {
			vm.pushUint64(top)
		}
	}

	return 0
}

// Process is a proxy passed to host functions in order to access
// things such as memory and control.
type Process struct {
	vm *VM
}

// NewProcess creates a VM interface object for host functions
func NewProcess(vm *VM) *Process {
	return &Process{vm: vm}
}

// ReadAt implements the ReaderAt interface: it copies into p
// the content of memory at offset off.
func (proc *Process) ReadAt(p []byte, off int64) (int, error) {
	mem, err := proc.vm.Memory().Project(int32(off), int32(len(p)))
	if err != nil {
		return 0, err
	}

	copy(p, mem)

	return len(p), nil
}

// WriteAt implements the WriterAt interface: it writes the content of p
// into the VM memory at offset off.
func (proc *Process) WriteAt(p []byte, off int64) (int, error) {
	mem, err := proc.vm.Memory().Project(int32(off), int32(len(p)))
	if err != nil {
		return 0, err
	}

	copy(mem, p)

	return len(p), nil
}

// Terminate stops the execution of the current module.
func (proc *Process) Terminate() {
	proc.vm.abort = true
}

type JmpBuf struct {
	Sp, Ip, Frame uint32
}

func (proc *Process) GetContext() *JmpBuf {
	return &JmpBuf{
		Sp:    uint32(proc.vm.frame.sp),
		Ip:    uint32(proc.vm.frame.ip),
		Frame: uint32(proc.vm.frameIdx),
	}
}

func (proc *Process) SetContext(jb *JmpBuf, val uint64) {
	if proc.vm.frameIdx < int(jb.Frame) {
		panic("longjmp wrong direction on stack")
	}

	frame := &proc.vm.frames[jb.Frame]

	frame.sp = int64(jb.Sp)
	frame.ip = int64(jb.Ip)

	proc.vm.frameIdx = int(jb.Frame)
	proc.vm.frame = frame

	proc.vm.pushUint64(val)
}