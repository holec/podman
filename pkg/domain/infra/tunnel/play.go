package tunnel

import (
	"context"

	"github.com/containers/image/v5/types"
	"github.com/containers/podman/v4/pkg/bindings/play"
	"github.com/containers/podman/v4/pkg/domain/entities"
)

func (ic *ContainerEngine) PlayKube(ctx context.Context, path string, opts entities.PlayKubeOptions) (*entities.PlayKubeReport, error) {
	options := new(play.KubeOptions).WithAuthfile(opts.Authfile).WithUsername(opts.Username).WithPassword(opts.Password)
	options.WithCertDir(opts.CertDir).WithQuiet(opts.Quiet).WithSignaturePolicy(opts.SignaturePolicy).WithConfigMaps(opts.ConfigMaps)
	options.WithLogDriver(opts.LogDriver).WithNetwork(opts.Networks).WithSeccompProfileRoot(opts.SeccompProfileRoot)
	options.WithStaticIPs(opts.StaticIPs).WithStaticMACs(opts.StaticMACs)
	if len(opts.LogOptions) > 0 {
		options.WithLogOptions(opts.LogOptions)
	}
	if opts.Annotations != nil {
		options.WithAnnotations(opts.Annotations)
	}
	options.WithNoHosts(opts.NoHosts)
	if s := opts.SkipTLSVerify; s != types.OptionalBoolUndefined {
		options.WithSkipTLSVerify(s == types.OptionalBoolTrue)
	}
	if start := opts.Start; start != types.OptionalBoolUndefined {
		options.WithStart(start == types.OptionalBoolTrue)
	}
	return play.Kube(ic.ClientCtx, path, options)
}

func (ic *ContainerEngine) PlayKubeDown(ctx context.Context, path string, _ entities.PlayKubeDownOptions) (*entities.PlayKubeReport, error) {
	return play.KubeDown(ic.ClientCtx, path)
}
