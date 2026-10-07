//go:build !no_mcp

package mcp

func (s *Server) registerTools() {
	s.registerSchemaTools()
	s.registerRecordTools()
	s.registerOpsTools()
}
