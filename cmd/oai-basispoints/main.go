package main

import (
	"github.com/JaxsonWang/cpa-plugin-oai-basispoints-sub2api/internal/transport"
	pluginv1 "github.com/JaxsonWang/cpa-plugin-oai-basispoints-sub2api/pkg/pluginapi/v1"
)

func main() {
	pluginv1.Serve(transport.NewServer())
}
