package sink

import (
	"fmt"
	"strings"

	"github.com/bobg/go-generics/v2/slices"
	"github.com/streamingfast/substreams/manifest"
	pbsubstreams "github.com/streamingfast/substreams/pb/sf/substreams/v1"
	"go.uber.org/zap"
)

func ReadManifestAndModule(manifestPath string, outputModuleName string, expectedOutputModuleType string, zlog *zap.Logger) (
	pkg *pbsubstreams.Package,
	module *pbsubstreams.Module,
	outputModuleHash manifest.ModuleHash,
	err error,
) {
	zlog.Info("reading substreams manifest", zap.String("manifest_path", manifestPath))
	reader, err := manifest.NewReader(manifestPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("manifest reader: %w", err)
	}

	pkg, err = reader.Read()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("read manifest: %w", err)
	}

	graph, err := manifest.NewModuleGraph(pkg.Modules.Modules)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create substreams module graph: %w", err)
	}

	resolvedOutputModuleName := outputModuleName
	if resolvedOutputModuleName == InferOutputModuleFromPackage {
		zlog.Debug("inferring module output name from package directly")
		if pkg.SinkModule == "" {
			return nil, nil, nil, fmt.Errorf("sink module is required in sink config")
		}

		resolvedOutputModuleName = pkg.SinkModule
	}

	zlog.Info("finding output module", zap.String("module_name", resolvedOutputModuleName))
	module, err = graph.Module(resolvedOutputModuleName)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get output module %q: %w", resolvedOutputModuleName, err)
	}
	if module.GetKindMap() == nil {
		return nil, nil, nil, fmt.Errorf("ouput module %q is *not* of  type 'Mapper'", resolvedOutputModuleName)
	}

	zlog.Info("validating output module type", zap.String("module_name", module.Name), zap.String("module_type", module.Output.Type))

	if expectedOutputModuleType != IgnoreOutputModuleType && expectedOutputModuleType != "" {
		unprefixedExpectedTypes, prefixedExpectedTypes := sanitizeModuleTypes(expectedOutputModuleType)
		unprefixedActualType, prefixedActualType := sanitizeModuleType(module.Output.Type)

		if !slices.Contains(prefixedExpectedTypes, prefixedActualType) {
			return nil, nil, nil, fmt.Errorf("sink only supports map module with output type %q but selected module %q output type is %q", strings.Join(unprefixedExpectedTypes, ", "), module.Name, unprefixedActualType)
		}
	}

	hashes := manifest.NewModuleHashes()
	outputModuleHash, err = hashes.HashModule(pkg.Modules, module, graph)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("hash module %q: %w", module.Name, err)
	}

	return pkg, module, outputModuleHash, nil
}

// sanitizeModuleTypes has the same behavior as sanitizeModuleType but explodes
// the inpput string on comma and returns a slice of unprefixed and prefixed
// types for each of the input types.
func sanitizeModuleTypes(in string) (unprefixed, prefixed []string) {
	slices.Each(strings.Split(in, ","), func(in string) {
		unprefixedType, prefixedType := sanitizeModuleType(strings.TrimSpace(in))
		unprefixed = append(unprefixed, unprefixedType)
		prefixed = append(prefixed, prefixedType)
	})

	return
}

// sanitizeModuleType give back both prefixed (so with `proto:`) and unprefixed
// version of the input string:
//
// - `sanitizeModuleType("com.acme") == (com.acme, proto:com.acme)`
// - `sanitizeModuleType("proto:com.acme") == (com.acme, proto:com.acme)`
func sanitizeModuleType(in string) (unprefixed, prefixed string) {
	if strings.HasPrefix(in, "proto:") {
		return strings.TrimPrefix(in, "proto:"), in
	}

	return in, "proto:" + in
}

type expectedModuleType string

func (e expectedModuleType) String() string {
	if e == expectedModuleType(IgnoreOutputModuleType) {
		return "<Ignored>"
	}

	return string(e)
}