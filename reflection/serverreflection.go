package reflection

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"sync"

	"github.com/golang/protobuf/proto"
	dpb "github.com/golang/protobuf/protoc-gen-go/descriptor"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
)

type serverReflectionServer struct {
	s *grpc.Server
	// TODO mu is not used. Add lock() and unlock().
	mu                sync.Mutex
	typeToNameMap     map[reflect.Type]string
	nameToTypeMap     map[string]reflect.Type
	typeToFileDescMap map[reflect.Type]*dpb.FileDescriptorProto
	// TODO remove this, replace with s.ftdmap
	filenameToDescMap map[string]*dpb.FileDescriptorProto
}

// InstallOnServer installs server reflection service on the given grpc server.
func InstallOnServer(s *grpc.Server) {
	rpb.RegisterServerReflectionServer(s, &serverReflectionServer{
		s:                 s,
		typeToNameMap:     make(map[reflect.Type]string),
		nameToTypeMap:     make(map[string]reflect.Type),
		typeToFileDescMap: make(map[reflect.Type]*dpb.FileDescriptorProto),
		filenameToDescMap: make(map[string]*dpb.FileDescriptorProto),
	})
}

type protoMessage interface {
	Descriptor() ([]byte, []int)
}

func (s *serverReflectionServer) fileDescForType(st reflect.Type) (*dpb.FileDescriptorProto, []int, error) {
	// Indexes list is not stored in cache.
	// So this step is needed to get idxs.
	m, ok := reflect.Zero(reflect.PtrTo(st)).Interface().(protoMessage)
	if !ok {
		return nil, nil, fmt.Errorf("failed to create message from type: %v", st)
	}
	enc, idxs := m.Descriptor()

	// Check type to fileDesc cache.
	if fd, ok := s.typeToFileDescMap[st]; ok {
		return fd, idxs, nil
	}

	// Cache missed, try to decode.
	fd, err := s.decodeFileDesc(enc)
	if err != nil {
		return nil, nil, err
	}
	// Add to cache.
	s.typeToFileDescMap[st] = fd
	return fd, idxs, nil
}

func (s *serverReflectionServer) decodeFileDesc(enc []byte) (*dpb.FileDescriptorProto, error) {
	raw := decompress(enc)
	if raw == nil {
		return nil, fmt.Errorf("failed to decompress enc")
	}

	fd := new(dpb.FileDescriptorProto)
	if err := proto.Unmarshal(raw, fd); err != nil {
		return nil, fmt.Errorf("bad descriptor: %v", err)
	}
	// If decodeFileDesc is called, it's the first time this file is seen.
	// Add it to cache.
	s.filenameToDescMap[fd.GetName()] = fd
	return fd, nil
}

func decompress(b []byte) []byte {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		fmt.Printf("bad gzipped descriptor: %v\n", err)
		return nil
	}
	out, err := ioutil.ReadAll(r)
	if err != nil {
		fmt.Printf("bad gzipped descriptor: %v\n", err)
		return nil
	}
	return out
}

func (s *serverReflectionServer) typeForName(name string) (reflect.Type, error) {
	// Check cache first.
	if st, ok := s.nameToTypeMap[name]; ok {
		return st, nil
	}

	pt := proto.MessageType(name)
	if pt == nil {
		return nil, fmt.Errorf("unknown type: %q", name)
	}
	st := pt.Elem()

	// Add to cache.
	s.typeToNameMap[st] = name
	s.nameToTypeMap[name] = st

	// TODO is this necessary?
	// In most cases, the returned type will be used to search
	// for file descriptor.
	// Add it to cache now.
	fd, _, err := s.fileDescForType(st)
	if err == nil {
		s.typeToFileDescMap[st] = fd
	}

	return st, nil
}

func (s *serverReflectionServer) nameForType(st reflect.Type) (string, error) {
	// Check cache first.
	if name, ok := s.typeToNameMap[st]; ok {
		return name, nil
	}

	var name string
	fd, idxs, err := s.fileDescForType(st)
	if err != nil {
		return "", err
	}
	mt := fd.MessageType[idxs[0]]
	name = mt.GetName()
	for i := 1; i < len(idxs); i++ {
		mt = mt.NestedType[idxs[i]]
		name += "_" + mt.GetName()
	}
	if fd.Package != nil {
		name = *fd.Package + "." + name
	}

	// Add to cache.
	s.typeToNameMap[st] = name
	s.nameToTypeMap[name] = st

	return name, nil
}

func (s *serverReflectionServer) nameForPointer(i interface{}) (string, error) {
	return s.nameForType(reflect.TypeOf(i).Elem())
}

func (s *serverReflectionServer) filenameForType(st reflect.Type) (string, error) {
	// Check cache first. The value of cache is descriptor, not filename.
	if fd, ok := s.typeToFileDescMap[st]; ok {
		return fd.GetName(), nil
	}

	fd, _, err := s.fileDescForType(st)
	if err != nil {
		return "", err
	}
	return fd.GetName(), nil
}

// TODO filenameForMethod and Service

func (s *serverReflectionServer) fileDescContainingExtension(st reflect.Type, ext int32) (*dpb.FileDescriptorProto, error) {
	m, ok := reflect.Zero(reflect.PtrTo(st)).Interface().(proto.Message)
	if !ok {
		return nil, fmt.Errorf("failed to create message from type: %v", st)
	}

	var extDesc *proto.ExtensionDesc
	for id, desc := range proto.RegisteredExtensions(m) {
		if id == ext {
			extDesc = desc
			break
		}
	}

	if extDesc == nil {
		return nil, fmt.Errorf("failed to find registered extension for extension number %v", ext)
	}

	extT := reflect.TypeOf(extDesc.ExtensionType).Elem()
	// TODO this doesn't work if extT is simple types, like int32
	// Check cache.
	if fd, ok := s.typeToFileDescMap[extT]; ok {
		return fd, nil
	}

	fd, _, err := s.fileDescForType(extT)
	if err != nil {
		return nil, err
	}
	return fd, nil
}

// TODO filenameContainingExtension
// fd := fileDescContainingExtension()
// return fd.GetName()

// fileDescWireFormatByFilename returns the file descriptor of file with the given name.
// TODO exporte and add lock
func (s *serverReflectionServer) fileDescWireFormatByFilename(name string) ([]byte, error) {
	fd, ok := s.filenameToDescMap[name]
	if !ok {
		return nil, fmt.Errorf("unknown file: %v", name)
	}
	b, err := proto.Marshal(fd)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s *serverReflectionServer) fileDescWireFormatContainingSymbol(name string) ([]byte, error) {
	var (
		fd *dpb.FileDescriptorProto
	)
	// Check if it's a type name.
	if st, err := s.typeForName(name); err == nil {
		fd, _, err = s.fileDescForType(st)
		if err != nil {
			return nil, err
		}
	} else {
		// Check if it's a service name or method name.
		meta := s.s.Metadata(name)
		if meta != nil {
			if enc, ok := meta.([]byte); ok {
				fd, err = s.decodeFileDesc(enc)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	// Marshal to wire format.
	if fd != nil {
		b, err := proto.Marshal(fd)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	return nil, fmt.Errorf("unknown symbol: %v", name)
}

func (s *serverReflectionServer) fileDescWireFormatContainingExtension(typeName string, extNum int32) ([]byte, error) {
	st, err := s.typeForName(typeName)
	if err != nil {
		return nil, err
	}
	fd, err := s.fileDescContainingExtension(st, extNum)
	if err != nil {
		return nil, err
	}
	b, err := proto.Marshal(fd)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (s *serverReflectionServer) allExtensionNumbersForType(st reflect.Type) ([]int32, error) {
	m, ok := reflect.Zero(reflect.PtrTo(st)).Interface().(proto.Message)
	if !ok {
		return nil, fmt.Errorf("failed to create message from type: %v", st)
	}

	var out []int32
	for id := range proto.RegisteredExtensions(m) {
		out = append(out, id)
	}
	return out, nil
}

func (s *serverReflectionServer) ServerReflectionInfo(stream rpb.ServerReflection_ServerReflectionInfoServer) error {
	for {
		in, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		out := &rpb.ServerReflectionResponse{
			ValidHost:       in.Host,
			OriginalRequest: in,
		}
		switch req := in.MessageRequest.(type) {
		case *rpb.ServerReflectionRequest_FileByFilename:
			b, err := s.fileDescWireFormatByFilename(req.FileByFilename)
			if err != nil {
				out.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &rpb.ErrorResponse{
						ErrorCode:    int32(codes.NotFound),
						ErrorMessage: err.Error(),
					},
				}
			} else {
				out.MessageResponse = &rpb.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &rpb.FileDescriptorResponse{FileDescriptorProto: [][]byte{b}},
				}
			}
		case *rpb.ServerReflectionRequest_FileContainingSymbol:
			b, err := s.fileDescWireFormatContainingSymbol(req.FileContainingSymbol)
			if err != nil {
				out.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &rpb.ErrorResponse{
						ErrorCode:    int32(codes.NotFound),
						ErrorMessage: err.Error(),
					},
				}
			} else {
				out.MessageResponse = &rpb.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &rpb.FileDescriptorResponse{FileDescriptorProto: [][]byte{b}},
				}
			}
		case *rpb.ServerReflectionRequest_FileContainingExtension:
			typeName := req.FileContainingExtension.ContainingType
			extNum := req.FileContainingExtension.ExtensionNumber
			b, err := s.fileDescWireFormatContainingExtension(typeName, extNum)
			if err != nil {
				out.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &rpb.ErrorResponse{
						ErrorCode:    int32(codes.NotFound),
						ErrorMessage: err.Error(),
					},
				}
			} else {
				out.MessageResponse = &rpb.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &rpb.FileDescriptorResponse{FileDescriptorProto: [][]byte{b}},
				}
			}
		case *rpb.ServerReflectionRequest_AllExtensionNumbersOfType:
		case *rpb.ServerReflectionRequest_ListServices:
		default:
			return grpc.Errorf(codes.InvalidArgument, "invalid MessageRequest: %v", in.MessageRequest)
		}

		if err := stream.Send(out); err != nil {
			return err
		}
	}
}
