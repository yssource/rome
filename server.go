// Copyright 2020 Staysail Systems, Inc. <info@staysail.tech>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use file except in compliance with the License.
// You may obtain a copy of the license at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rome

import (
	"bytes"
	"errors"
	"reflect"
	"sync"
	"unicode"

	"github.com/vmihailenco/msgpack"
	"nanomsg.org/go/mangos/v2"
	"nanomsg.org/go/mangos/v2/protocol/rep"

	_ "nanomsg.org/go/mangos/v2/transport/all"
)

// This relates to the RPC rpcServer.

type RpcServer interface {

	// Dial is used to dial a remote server.  This may be called multiple
	// times to dial to different servers.  If multiple connections are
	// present, then the client will automatically select the best
	// one based on readiness to service the request.
	Dial(url string, opts ...interface{}) error

	// Listen is much like dial, but acts as a server.  This allows
	// the normal server/client roles to be reversed while still
	// maintaining the REQ/REP higher level roles.  It is possible
	// to freely mix and match multiple calls of Listen, with or without
	// calls to Dial.
	Listen(url string, opts ...interface{}) error

	// SetOption sets global options on the server, such as retry times.
	SetOption(opts ...interface{}) error

	// Close closes down the socket.  In-flight requests will be aborted
	// and return accordingly.
	Close()

	// Register registers an object instance.  Each method that meets the
	// necessary criteria is registered using the name "<type>.method"
	// The type must be an exported type.
	Register(obj interface{}) error

	// Serve serves one context synchronously.  This is the simplest
	// and least form of service, as it runs utterly synchronously.
	Serve()

	// ServeAsync serves asynchronously, firing off the given number
	// of go routines, each with their own context, in parallel.
	// It returns immediately.
	ServeAsync(workers int)
}

func NewRpcServer() RpcServer {
	s := &rpcServer{}
	s.socket, _ = rep.NewSocket()
	s.methods = make(map[string]*rpcMethod)
	return s
}

type rpcMethod struct {
	fn       func(interface{}, interface{}) error
	typ      reflect.Type
	val      reflect.Value
	receiver reflect.Value
	argType  reflect.Type
	resType  reflect.Type
}

type rpcServer struct {
	socket  mangos.Socket
	lock    sync.Mutex
	methods map[string]*rpcMethod
}

func (s *rpcServer) Close() {
	_ = s.socket.Close()
}

// NB: We don't use the mangos timeout options.  Instead we rely on the
// context to provide a global timeout which encompasses both the send
// and the receive time.

func (s *rpcServer) SetOption(opts ...interface{}) error {
	for _, o := range opts {
		switch v := o.(type) {
		case OptionOther:
			return s.socket.SetOption(v.Name, v.Value)
		case OptionDialAsync:
			return s.socket.SetOption(mangos.OptionDialAsynch, v)
		case OptionReconnectTime:
			return s.socket.SetOption(mangos.OptionReconnectTime, v)
		case OptionMaxReconnectTime:
			return s.socket.SetOption(mangos.OptionMaxReconnectTime, v)
		case OptionTLSConfig:
			return s.socket.SetOption(mangos.OptionTLSConfig, v)
		default:
			return errors.New("unknown option")
		}
	}
	return nil
}

func (s *rpcServer) Dial(url string, opts ...interface{}) error {
	d, e := s.socket.NewDialer(url, nil)
	if e != nil {
		return e
	}
	for _, o := range opts {

		switch v := o.(type) {
		case OptionOther:
			e = d.SetOption(v.Name, v.Value)
			if e != nil {
				return e
			}
		case OptionDialAsync:
			e = d.SetOption(mangos.OptionDialAsynch, v)
			if e != nil {
				return e
			}
		case OptionReconnectTime:
			e = d.SetOption(mangos.OptionReconnectTime, v)
			if e != nil {
				return e
			}
		case OptionMaxReconnectTime:
			e = d.SetOption(mangos.OptionMaxReconnectTime, v)
			if e != nil {
				return e
			}
		case OptionTLSConfig:
			e = d.SetOption(mangos.OptionTLSConfig, v)
			if e != nil {
				return e
			}
		default:
			return errors.New("unknown option")
		}
	}

	return d.Dial()
}

func (s *rpcServer) Listen(url string, opts ...interface{}) error {
	l, e := s.socket.NewListener(url, nil)
	if e != nil {
		return e
	}
	for _, o := range opts {
		switch v := o.(type) {
		case OptionTLSConfig:
			e = l.SetOption(mangos.OptionTLSConfig, v)
			if e != nil {
				return e
			}
		case OptionOther:
			e = l.SetOption(v.Name, v.Value)
			if e != nil {
				return e
			}
		default:
			return errors.New("unknown option")
		}
	}
	return l.Listen()
}

func sendErr(c mangos.Context, err *Error) {
	var buf = &bytes.Buffer{}
	enc := msgpack.NewEncoder(buf)
	if enc.EncodeArrayLen(3) != nil || // array header
		enc.EncodeUint8(1) != nil || // version
		enc.EncodeBool(false) != nil || // false
		enc.EncodeArrayLen(3) != nil ||
		enc.EncodeInt(int64(err.Code)) != nil ||
		enc.EncodeString(err.Message) != nil ||
		enc.Encode(err) != nil {
		return
	}

	// If the send fails, we will just move onto the next request.
	// If the context is closed, we'll figure it out when we try to recv.
	_ = c.Send(buf.Bytes())
}

func (s *rpcServer) serveContext(c mangos.Context) {
	for {
		b, e := c.Recv()
		if e != nil {
			// the only time this fails its due to closed context.
			// bail in that case.
			break
		}
		// Now we are going to do an inline decode.  We decode
		// piecewise to allow for polymorphism in the message.
		dec := msgpack.NewDecoder(bytes.NewReader(b))

		l, e := dec.DecodeArrayLen()
		if e != nil {
			sendErr(c, NewError(ErrParse, "message not an array?", e.Error()))
			continue
		}
		if l != 3 {
			sendErr(c, NewError(ErrInvalidRequest, "message array length invalid", nil))
			continue
		}

		// Decode version which must be one.
		ver, e := dec.DecodeUint8()
		if e != nil {
			sendErr(c, NewError(ErrParse, "unable to parse version", e.Error()))
			continue
		}
		if ver != 1 {
			sendErr(c, NewError(ErrBadVersion, "bad version (must be 1)", ver))
			continue
		}

		// Decode method name.  In the future we might allow decoding
		// methods by number.
		name, e := dec.DecodeString()
		if e != nil {
			sendErr(c, NewError(ErrParse, "unable to parse method name", e.Error()))
			continue
		}
		if name == "" {
			sendErr(c, NewError(ErrMethodNotFound, "method name empty", nil))
			continue
		}

		// Now lookup method
		s.lock.Lock()
		m, ok := s.methods[name]
		s.lock.Unlock()

		if !ok || m == nil {
			sendErr(c, NewError(ErrMethodNotFound, "method not found", nil))
			continue
		}

		args := make([]reflect.Value, 0, 3)

		if m.receiver.IsValid() {
			args = append(args, m.receiver)
		}

		arg := reflect.New(m.argType.Elem())
		if e = dec.DecodeValue(arg); e != nil {
			sendErr(c, NewError(ErrInvalidParams, "failed decoding arguments", e.Error()))
			continue
		}
		args = append(args, arg)

		result := reflect.New(m.resType.Elem())
		args = append(args, result)

		rv := m.val.Call(args)
		// len(rv) must be 1 -- this could be an assert.
		if !rv[0].IsNil() {
			e = rv[0].Interface().(error)
			if e != nil {
				sendErr(c, ErrorWrap(e))
				continue
			}
		}

		buf := &bytes.Buffer{}
		enc := msgpack.NewEncoder(buf)

		if enc.EncodeArrayLen(3) != nil ||
			enc.EncodeUint8(1) != nil || // version
			enc.EncodeBool(true) != nil { // success
			// this really should never happen
			sendErr(c, NewError(ErrInternal, "failed to marshal header", nil))
			continue
		}

		if e := enc.EncodeValue(result); e != nil {
			sendErr(c, NewError(ErrInternal, "failed to marshal result", e.Error()))
			continue
		}

		_ = c.Send(buf.Bytes())
	}
}

func (s *rpcServer) ServeAsync(num int) {
	for i := 0; i < num; i++ {
		go func() {
			s.Serve()
		}()
	}
}

func (s *rpcServer) Serve() {
	c, e := s.socket.OpenContext()
	if e != nil {
		return
	}
	s.serveContext(c)
	_ = c.Close()
}

func isExported(name string) bool {
	for _, r := range name {
		if unicode.IsUpper(r) {
			return true
		}
		return false
	}
	return false
}

func (s *rpcServer) registerValue(name string, receiver reflect.Value, methodValue reflect.Value, methodType reflect.Type) error {
	m := &rpcMethod{}

	if name == "" {
		return errors.New("missing name")
	}

	println("Registering", name)

	if methodValue.Kind() != reflect.Func {
		return errors.New("handler not a method or function")
	}
	if methodType.NumOut() != 1 || methodType.Out(0).Name() != "error" {
		return errors.New("bad signature, func must return error")
	}

	m.receiver = receiver
	m.val = methodValue
	m.typ = methodType

	if receiver.IsValid() {
		if methodType.NumIn() != 3 {
			return errors.New("bad signature, func must take 2 pointer arguments")
		}
		m.argType = methodType.In(1)
		m.resType = methodType.In(2)
	} else {
		if methodType.NumIn() != 2 {
			return errors.New("bad signature, func must take 2 pointer arguments")
		}
		m.argType = methodType.In(0)
		m.resType = methodType.In(1)
	}

	if m.argType.Kind() != reflect.Ptr || m.resType.Kind() != reflect.Ptr {
		return errors.New("bad signature, func must take 2 pointer arguments")
	}

	s.lock.Lock()
	// This overwrites any prior instance
	s.methods[name] = m
	s.lock.Unlock()
	return nil
}

// RegisterFuncName registers a bare function (no receiver or receiver is
// implied), along with a specific name for the method.  The function fn
// must have signature func(args *ArgType, result *resultType) error.
func (s *rpcServer) RegisterFuncName(name string, fn interface{}) error {
	var receiver reflect.Value
	funcVal := reflect.ValueOf(fn)
	funcType := reflect.TypeOf(fn)
	return s.registerValue(name, receiver, funcVal, funcType)
}

// RegisterFunc registers a function using the name of the function.
// The function must be exported, and have signature
// func(args *ArgType, result *resultType) error.
func (s *rpcServer) RegisterFunc(fn interface{}) error {
	var receiver reflect.Value
	funcVal := reflect.ValueOf(fn)
	funcType := reflect.TypeOf(fn)

	name := funcType.Name()
	if !isExported(name) {
		return errors.New("function is not exported")
	}
	return s.registerValue(name, receiver, funcVal, funcType)
}

// RegisterName registers every method of obj that matches the
// signature "func (F)(args T1, results T2) error" where F is a public
// name, T1 is a pointer, and T2 is a pointer.  The method names will
// be registered as "name.F" where F is the method name.
func (s *rpcServer) RegisterName(name string, obj interface{}) error {
	t := reflect.TypeOf(obj)
	v := reflect.ValueOf(obj)

	for i := 0; i < t.NumMethod(); i++ {
		m := t.Method(i)

		// if the name is empty, then we don't qualify.
		var mn string
		if name == "" {
			mn = m.Name
		} else {
			mn = name + "." + m.Name
		}

		e := s.registerValue(mn, v, m.Func, m.Type)
		if e != nil {
			return e
		}
	}
	return nil
}

// Register registers an object instance.  Each method that meets the
// necessary criteria is registered using the name "<type>.method"
// The type must be an exported type.
func (s *rpcServer) Register(obj interface{}) error {
	v := reflect.ValueOf(obj)
	name := reflect.Indirect(v).Type().Name()
	if !isExported(name) {
		return errors.New("receiver type not exported")
	}
	return s.RegisterName(name, obj)
}
