package commands

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	cid "github.com/ipfs/go-cid"
	cmds "github.com/ipfs/go-ipfs-cmds"
	config "github.com/ipfs/go-ipfs-config"
	"github.com/ipfs/go-ipfs/core/commands/cmdenv"
	fsrepo "github.com/ipfs/go-ipfs/repo/fsrepo"
	pinclient "github.com/ipfs/go-pinning-service-http-client"
	path "github.com/ipfs/interface-go-ipfs-core/path"
	peer "github.com/libp2p/go-libp2p-core/peer"
	"github.com/multiformats/go-multiaddr"
)

var remotePinCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline: "Pin (and unpin) objects to remote pinning service.",
	},

	Subcommands: map[string]*cmds.Command{
		"add":     addRemotePinCmd,
		"ls":      listRemotePinCmd,
		"rm":      rmRemotePinCmd,
		"service": remotePinServiceCmd,
	},
}

var remotePinServiceCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline: "Configure remote pinning services.",
	},

	Subcommands: map[string]*cmds.Command{
		"add":    addRemotePinServiceCmd,
		"ls":     lsRemotePinServiceCmd,
		"rename": renameRemotePinServiceCmd,
		"rm":     rmRemotePinServiceCmd,
		"update": updateRemotePinServiceCmd,
	},
}

const pinNameOptionName = "name"
const pinCIDsOptionName = "cid"
const pinStatusOptionName = "status"
const pinServiceNameOptionName = "service"
const pinBackgroundOptionName = "background"

type RemotePinOutput struct {
	RequestID string
	Name      string
	Delegates []string // multiaddr
	Status    string
	Cid       string
}

// remote pin commands

var addRemotePinCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline:          "Pin objects to remote storage.",
		ShortDescription: "Stores an IPFS object(s) from a given path to a remote pinning service.",
	},

	Arguments: []cmds.Argument{
		cmds.StringArg("ipfs-path", true, false, "Path to object(s) to be pinned.").EnableStdin(),
	},
	Options: []cmds.Option{
		cmds.StringOption(pinNameOptionName, "An optional name for the pin."),
		cmds.StringOption(pinServiceNameOptionName, "Name of the remote pinning service to use."),
		cmds.BoolOption(pinBackgroundOptionName, "Add the pins in the background.").WithDefault(true),
	},
	Type: RemotePinOutput{},
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) error {
		ctx, cancel := context.WithCancel(req.Context)
		defer cancel()

		opts := []pinclient.AddOption{}
		if name, nameFound := req.Options[pinNameOptionName].(string); nameFound {
			opts = append(opts, pinclient.PinOpts.WithName(name))
		}

		api, err := cmdenv.GetApi(env, req)
		if err != nil {
			return err
		}

		if len(req.Arguments) != 1 {
			return fmt.Errorf("expecting one CID argument")
		}
		rp, err := api.ResolvePath(ctx, path.New(req.Arguments[0]))
		if err != nil {
			return err
		}

		service, _ := req.Options[pinServiceNameOptionName].(string)
		c, err := getRemotePinServiceOrEnv(env, service)
		if err != nil {
			return err
		}

		ps, err := c.Add(ctx, rp.Cid(), opts...)
		if err != nil {
			return err
		}

		for _, d := range ps.GetDelegates() {
			p, err := peer.AddrInfoFromP2pAddr(d)
			if err != nil {
				return err
			}
			if err := api.Swarm().Connect(ctx, *p); err != nil {
				log.Infof("error connecting to remote pin delegate %v : %w", d, err)
			}

		}

		if !req.Options[pinBackgroundOptionName].(bool) {
			for {
				ps, err = c.GetStatusByID(ctx, ps.GetRequestId())
				if err != nil {
					return fmt.Errorf("failed to query pin (%v)", err)
				}
				s := ps.GetStatus()
				if s == pinclient.StatusPinned {
					break
				}
				if s == pinclient.StatusFailed {
					return fmt.Errorf("failed to pin")
				}
				tmr := time.NewTimer(time.Second / 2)
				select {
				case <-tmr.C:
				case <-ctx.Done():
					return fmt.Errorf("waiting for pin interrupted")
				}
			}
		}

		return res.Emit(&RemotePinOutput{
			RequestID: ps.GetRequestId(),
			Name:      ps.GetPin().GetName(),
			Delegates: multiaddrsToStrings(ps.GetDelegates()),
			Status:    ps.GetStatus().String(),
			Cid:       ps.GetPin().GetCid().String(),
		})
	},
	Encoders: cmds.EncoderMap{
		cmds.Text: cmds.MakeTypedEncoder(func(req *cmds.Request, w io.Writer, out *RemotePinOutput) error {
			fmt.Printf("pin_id=%v\n", out.RequestID)
			fmt.Printf("pin_name=%q\n", out.Name)
			for _, d := range out.Delegates {
				fmt.Printf("pin_delegate=%v\n", d)
			}
			fmt.Printf("pin_status=%v\n", out.Status)
			fmt.Printf("pin_cid=%v\n", out.Cid)
			return nil
		}),
	},
}

func multiaddrsToStrings(m []multiaddr.Multiaddr) []string {
	r := make([]string, len(m))
	for i := range m {
		r[i] = m[i].String()
	}
	return r
}

var listRemotePinCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline: "List objects pinned to remote pinning service.",
		ShortDescription: `
Returns a list of objects that are pinned to a remote pinning service.
`,
		LongDescription: `
Returns a list of objects that are pinned to a remote pinning service.
`,
	},

	Arguments: []cmds.Argument{},
	Options: []cmds.Option{
		cmds.StringOption(pinNameOptionName, "Return pins objects with names that contain provided value (case-sensitive, exact match)."),
		cmds.StringsOption(pinCIDsOptionName, "Return only pin objects for the specified CID(s); optional, comma separated."),
		cmds.StringsOption(pinStatusOptionName, "Return only pin objects with the specified statuses; optional, comma separated."),
		cmds.StringOption(pinServiceNameOptionName, "Name of the remote pinning service to use."),
	},
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) error {
		ctx, cancel := context.WithCancel(req.Context)
		defer cancel()

		psCh, errCh, err := lsRemote(ctx, req, env)
		if err != nil {
			return err
		}

		for ps := range psCh {
			if err := res.Emit(&RemotePinOutput{
				RequestID: ps.GetRequestId(),
				Name:      ps.GetPin().GetName(),
				Delegates: multiaddrsToStrings(ps.GetDelegates()),
				Status:    ps.GetStatus().String(),
				Cid:       ps.GetPin().GetCid().String(),
			}); err != nil {
				return err
			}
		}

		return <-errCh
	},
	Type: RemotePinOutput{},
	Encoders: cmds.EncoderMap{
		cmds.Text: cmds.MakeTypedEncoder(func(req *cmds.Request, w io.Writer, out *RemotePinOutput) error {
			fmt.Printf("pin_id=%v\n", out.RequestID)
			fmt.Printf("pin_name=%q\n", out.Name)
			for _, d := range out.Delegates {
				fmt.Printf("pin_delegate=%v\n", d)
			}
			fmt.Printf("pin_status=%v\n", out.Status)
			fmt.Printf("pin_cid=%v\n", out.Cid)
			return nil
		}),
	},
}

func lsRemote(ctx context.Context, req *cmds.Request, env cmds.Environment) (chan pinclient.PinStatusGetter, chan error, error) {
	opts := []pinclient.LsOption{}
	if name, nameFound := req.Options[pinNameOptionName].(string); nameFound {
		opts = append(opts, pinclient.PinOpts.FilterName(name))
	}
	if cidsRaw, cidsFound := req.Options[pinCIDsOptionName].([]string); cidsFound {
		parsedCIDs := []cid.Cid{}
		for _, rawCID := range cidsRaw {
			parsedCID, err := cid.Decode(rawCID)
			if err != nil {
				return nil, nil, fmt.Errorf("CID %s cannot be parsed (%v)", rawCID, err)
			}
			parsedCIDs = append(parsedCIDs, parsedCID)
		}
		opts = append(opts, pinclient.PinOpts.FilterCIDs(parsedCIDs...))
	}
	if statusRaw, statusFound := req.Options[pinStatusOptionName].([]string); statusFound {
		parsedStatuses := []pinclient.Status{}
		for _, rawStatus := range statusRaw {
			s := pinclient.Status(rawStatus)
			if s.String() == string(pinclient.StatusUnknown) {
				return nil, nil, fmt.Errorf("status %s is not valid", rawStatus)
			}
			parsedStatuses = append(parsedStatuses, s)
		}
		opts = append(opts, pinclient.PinOpts.FilterStatus(parsedStatuses...))
	}

	service, _ := req.Options[pinServiceNameOptionName].(string)
	c, err := getRemotePinServiceOrEnv(env, service)
	if err != nil {
		return nil, nil, err
	}

	psCh, errCh := c.Ls(ctx, opts...)

	return psCh, errCh, nil
}

var rmRemotePinCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline: "Remove pinned objects from remote pinning service.",
		ShortDescription: `
Removes the pin from the given object allowing it to be garbage
collected if needed.
`,
	},

	Arguments: []cmds.Argument{
		cmds.StringArg("request-id", false, true, "Request ID of the pin to be removed.").EnableStdin(),
	},
	Options: []cmds.Option{
		cmds.StringOption(pinNameOptionName, "Remove pin objects with names that contain provided value (case-sensitive, exact match)."),
		cmds.StringsOption(pinCIDsOptionName, "Remove only pin objects for the specified CID(s); optional, comma separated."),
		cmds.StringsOption(pinStatusOptionName, "Remove only pin objects with the specified statuses; optional, comma separated."),
		cmds.StringOption(pinServiceNameOptionName, "Name of the remote pinning service to use."),
	},
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) error {
		ctx, cancel := context.WithCancel(req.Context)
		defer cancel()

		rmIDs := []string{}
		if len(req.Arguments) == 0 {
			psCh, errCh, err := lsRemote(ctx, req, env)
			if err != nil {
				return err
			}
			for ps := range psCh {
				rmIDs = append(rmIDs, ps.GetRequestId())
			}
			if err = <-errCh; err != nil {
				return fmt.Errorf("listing remote pin IDs (%v)", err)
			}
		} else {
			rmIDs = append(rmIDs, req.Arguments[0])
		}

		service, _ := req.Options[pinServiceNameOptionName].(string)
		c, err := getRemotePinServiceOrEnv(env, service)
		if err != nil {
			return err
		}

		for _, rmID := range rmIDs {
			if err = c.DeleteByID(ctx, rmID); err != nil {
				return fmt.Errorf("removing pin with request ID %s (%v)", rmID, err)
			}
		}
		return nil
	},
}

// remote service commands

var addRemotePinServiceCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline:          "Add remote pinning service.",
		ShortDescription: "Add a credentials for access to a remote pinning service.",
	},
	Arguments: []cmds.Argument{
		cmds.StringArg("ipfs-path", true, true, "Name, URL and key (in that order) for a remote pinning service.").EnableStdin(),
	},
	Options: []cmds.Option{},
	Type:    nil,
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) error {
		cfgRoot, err := cmdenv.GetConfigRoot(env)
		if err != nil {
			return err
		}
		repo, err := fsrepo.Open(cfgRoot)
		if err != nil {
			return err
		}
		defer repo.Close()

		if len(req.Arguments) != 3 {
			return fmt.Errorf("expecting three argument: name, url and key")
		}
		name := req.Arguments[0]
		url := req.Arguments[1]
		key := req.Arguments[2]

		cfg, err := repo.Config()
		if err != nil {
			return err
		}
		if cfg.RemotePinServices.Services != nil {
			if _, present := cfg.RemotePinServices.Services[name]; present {
				return fmt.Errorf("service already present")
			}
		} else {
			cfg.RemotePinServices.Services = map[string]config.RemotePinService{}
		}
		cfg.RemotePinServices.Services[name] = config.RemotePinService{
			Name: name,
			URL:  url,
			Key:  key,
		}

		return repo.SetConfig(cfg)
	},
}

var updateRemotePinServiceCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline:          "Update a remote pinning service.",
		ShortDescription: "Update credentials for access to a remote pinning service.",
	},
	Arguments: []cmds.Argument{
		cmds.StringArg("ipfs-path", true, true, "Name, new URL and new key (in that order) for a remote pinning service.").EnableStdin(),
	},
	Options: []cmds.Option{},
	Type:    nil,
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) error {
		cfgRoot, err := cmdenv.GetConfigRoot(env)
		if err != nil {
			return err
		}
		repo, err := fsrepo.Open(cfgRoot)
		if err != nil {
			return err
		}
		defer repo.Close()

		if len(req.Arguments) != 3 {
			return fmt.Errorf("expecting three argument: name, url and key")
		}
		name := req.Arguments[0]
		url := req.Arguments[1]
		key := req.Arguments[2]

		cfg, err := repo.Config()
		if err != nil {
			return err
		}
		if cfg.RemotePinServices.Services == nil {
			return fmt.Errorf("service not found")
		}
		if _, present := cfg.RemotePinServices.Services[name]; !present {
			return fmt.Errorf("service not found")
		}
		cfg.RemotePinServices.Services[name] = config.RemotePinService{
			Name: name,
			URL:  url,
			Key:  key,
		}

		return repo.SetConfig(cfg)
	},
}

var rmRemotePinServiceCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline:          "Remove remote pinning service.",
		ShortDescription: "Remove credentials for access to a remote pinning service.",
	},
	Arguments: []cmds.Argument{
		cmds.StringArg("ipfs-path", true, false, "Name of remote pinning service to remove.").EnableStdin(),
	},
	Options: []cmds.Option{},
	Type:    nil,
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) error {
		cfgRoot, err := cmdenv.GetConfigRoot(env)
		if err != nil {
			return err
		}
		repo, err := fsrepo.Open(cfgRoot)
		if err != nil {
			return err
		}
		defer repo.Close()

		if len(req.Arguments) != 1 {
			return fmt.Errorf("expecting one argument: name")
		}
		name := req.Arguments[0]

		cfg, err := repo.Config()
		if err != nil {
			return err
		}
		if cfg.RemotePinServices.Services != nil {
			delete(cfg.RemotePinServices.Services, name)
		}
		return repo.SetConfig(cfg)
	},
}

var renameRemotePinServiceCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline:          "Rename a remote pinning service.",
		ShortDescription: "Rename a remote pinning service.",
	},
	Arguments: []cmds.Argument{
		cmds.StringArg("ipfs-path", true, true, "Old and new name of remote pinning service to rename.").EnableStdin(),
	},
	Options: []cmds.Option{},
	Type:    nil,
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) error {
		cfgRoot, err := cmdenv.GetConfigRoot(env)
		if err != nil {
			return err
		}
		repo, err := fsrepo.Open(cfgRoot)
		if err != nil {
			return err
		}
		defer repo.Close()

		if len(req.Arguments) != 2 {
			return fmt.Errorf("expecting two arguments: old name and new name")
		}
		oldName := req.Arguments[0]
		newName := req.Arguments[1]

		cfg, err := repo.Config()
		if err != nil {
			return err
		}
		if cfg.RemotePinServices.Services == nil {
			return fmt.Errorf("remote pinning service not found")
		}
		s, present := cfg.RemotePinServices.Services[oldName]
		if !present {
			return fmt.Errorf("remote pinning service not found")
		}
		delete(cfg.RemotePinServices.Services, oldName)
		s.Name = newName
		cfg.RemotePinServices.Services[newName] = s
		return repo.SetConfig(cfg)
	},
}

var lsRemotePinServiceCmd = &cmds.Command{
	Helptext: cmds.HelpText{
		Tagline:          "List remote pinning services.",
		ShortDescription: "List remote pinning services.",
	},
	Arguments: []cmds.Argument{},
	Options:   []cmds.Option{},
	Run: func(req *cmds.Request, res cmds.ResponseEmitter, env cmds.Environment) error {
		cfgRoot, err := cmdenv.GetConfigRoot(env)
		if err != nil {
			return err
		}
		repo, err := fsrepo.Open(cfgRoot)
		if err != nil {
			return err
		}
		defer repo.Close()

		cfg, err := repo.Config()
		if err != nil {
			return err
		}
		if cfg.RemotePinServices.Services == nil {
			return nil // no pinning services added yet
		}
		result := sortedServiceAndURL{}
		for svcName, svcConfig := range cfg.RemotePinServices.Services {
			result = append(result, PinServiceAndURL{svcName, svcConfig.URL})
		}
		sort.Sort(result)
		for _, r := range result {
			if err := res.Emit(r); err != nil {
				return err
			}
		}
		return nil
	},
	Type: PinServiceAndURL{},
}

type PinServiceAndURL struct {
	Service string
	URL     string
}

type sortedServiceAndURL []PinServiceAndURL

func (s sortedServiceAndURL) Len() int {
	return len(s)
}

func (s sortedServiceAndURL) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s sortedServiceAndURL) Less(i, j int) bool {
	return s[i].Service < s[j].Service
}

func getRemotePinServiceOrEnv(env cmds.Environment, name string) (*pinclient.Client, error) {
	if name == "" {
		return nil, fmt.Errorf("remote pinning service name not specified")
	}
	url, key, err := getRemotePinService(env, name)
	if err != nil {
		return nil, err
	}
	return pinclient.NewClient(url, key), nil
}

func getRemotePinService(env cmds.Environment, name string) (url, key string, err error) {
	cfgRoot, err := cmdenv.GetConfigRoot(env)
	if err != nil {
		return "", "", err
	}
	repo, err := fsrepo.Open(cfgRoot)
	if err != nil {
		return "", "", err
	}
	defer repo.Close()
	cfg, err := repo.Config()
	if err != nil {
		return "", "", err
	}
	if cfg.RemotePinServices.Services == nil {
		return "", "", fmt.Errorf("service not known")
	}
	service, present := cfg.RemotePinServices.Services[name]
	if !present {
		return "", "", fmt.Errorf("service not known")
	}
	return service.URL, service.Key, nil
}
