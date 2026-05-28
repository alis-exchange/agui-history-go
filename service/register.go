package service

import (
	pb "go.alis.build/common/alis/agui/history/v1"
	"google.golang.org/grpc"
)

// Register wires ThreadService into a gRPC server or any other ServiceRegistrar.
func (s *ThreadService) Register(registrar grpc.ServiceRegistrar) {
	pb.RegisterThreadServiceServer(registrar, s)
}
