// Copyright 2013 Joyent Inc.
// Licensed under the AGPLv3, see LICENCE file for details.

package joyent

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/joyent/gocommon/client"
	"github.com/joyent/gosdc/cloudapi"
	"github.com/juju/errors"
	"github.com/juju/names"
	"github.com/juju/utils"

	"github.com/juju/juju/apiserver/params"
	"github.com/juju/juju/constraints"
	"github.com/juju/juju/environs"
	"github.com/juju/juju/environs/imagemetadata"
	"github.com/juju/juju/environs/instances"
	"github.com/juju/juju/environs/simplestreams"
	"github.com/juju/juju/instance"
	"github.com/juju/juju/juju/arch"
	"github.com/juju/juju/network"
	"github.com/juju/juju/provider/common"
	"github.com/juju/juju/tools"
)

var (
	vTypeSmartmachine   = "smartmachine"
	vTypeVirtualmachine = "kvm"
	signedImageDataOnly = false
)

type joyentCompute struct {
	sync.Mutex
	ecfg     *environConfig
	cloudapi *cloudapi.Client
}

func newCompute(cfg *environConfig) (*joyentCompute, error) {
	creds, err := credentials(cfg)
	if err != nil {
		return nil, err
	}
	client := client.NewClient(cfg.sdcUrl(), cloudapi.DefaultAPIVersion, creds, &logger)

	return &joyentCompute{
		ecfg:     cfg,
		cloudapi: cloudapi.New(client)}, nil
}

func (env *joyentEnviron) machineFullName(machineId string) string {
	return fmt.Sprintf("juju-%s-%s", env.Config().Name(), names.NewMachineTag(machineId))
}

var unsupportedConstraints = []string{
	constraints.CpuPower,
	constraints.Tags,
}

// ConstraintsValidator is defined on the Environs interface.
func (env *joyentEnviron) ConstraintsValidator() (constraints.Validator, error) {
	validator := constraints.NewValidator()
	validator.RegisterUnsupported(unsupportedConstraints)
	supportedArches, err := env.SupportedArchitectures()
	if err != nil {
		return nil, err
	}
	validator.RegisterVocabulary(constraints.Arch, supportedArches)
	packages, err := env.compute.cloudapi.ListPackages(nil)
	if err != nil {
		return nil, err
	}
	instTypeNames := make([]string, len(packages))
	for i, pkg := range packages {
		instTypeNames[i] = pkg.Name
	}
	validator.RegisterVocabulary(constraints.InstanceType, instTypeNames)
	return validator, nil
}

func (env *joyentEnviron) StartInstance(args environs.StartInstanceParams) (instance.Instance, *instance.HardwareCharacteristics, []network.Info, error) {

	if args.MachineConfig.HasNetworks() {
		return nil, nil, nil, errors.New("starting instances with networks is not supported yet")
	}

	series := args.Tools.OneSeries()
	arches := args.Tools.Arches()
	spec, err := env.FindInstanceSpec(&instances.InstanceConstraint{
		Region:      env.Ecfg().Region(),
		Series:      series,
		Arches:      arches,
		Constraints: args.Constraints,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	tools, err := args.Tools.Match(tools.Filter{Arch: spec.Image.Arch})
	if err != nil {
		return nil, nil, nil, errors.Errorf("chosen architecture %v not present in %v", spec.Image.Arch, arches)
	}

	args.MachineConfig.Tools = tools[0]

	if err := environs.FinishMachineConfig(args.MachineConfig, env.Config()); err != nil {
		return nil, nil, nil, err
	}
	userData, err := environs.ComposeUserData(args.MachineConfig, nil)
	if err != nil {
		return nil, nil, nil, errors.Annotate(err, "cannot make user data")
	}

	// Unzipping as Joyent API expects it as string
	userData, err = utils.Gunzip(userData)
	if err != nil {
		return nil, nil, nil, errors.Annotate(err, "cannot make user data")
	}
	logger.Debugf("joyent user data: %d bytes", len(userData))

	var machine *cloudapi.Machine
	machine, err = env.compute.cloudapi.CreateMachine(cloudapi.CreateMachineOpts{
		//Name:	 env.machineFullName(machineConf.MachineId),
		Package:  spec.InstanceType.Name,
		Image:    spec.Image.Id,
		Metadata: map[string]string{"metadata.cloud-init:user-data": string(userData)},
		Tags:     map[string]string{"tag.group": "juju", "tag.env": env.Config().Name()},
	})
	if err != nil {
		return nil, nil, nil, errors.Annotate(err, "cannot create instances")
	}
	machineId := machine.Id

	logger.Infof("provisioning instance %q", machineId)

	machine, err = env.compute.cloudapi.GetMachine(machineId)
	if err != nil {
		return nil, nil, nil, errors.Annotate(err, "cannot start instances")
	}

	// wait for machine to start
	for !strings.EqualFold(machine.State, "running") {
		time.Sleep(1 * time.Second)

		machine, err = env.compute.cloudapi.GetMachine(machineId)
		if err != nil {
			return nil, nil, nil, errors.Annotate(err, "cannot start instances")
		}
	}

	logger.Infof("started instance %q", machineId)

	inst := &joyentInstance{
		machine: machine,
		env:     env,
	}

	if params.AnyJobNeedsState(args.MachineConfig.Jobs...) {
		if err := common.AddStateInstance(env.Storage(), inst.Id()); err != nil {
			logger.Errorf("could not record instance in provider-state: %v", err)
		}
	}

	disk64 := uint64(machine.Disk)
	hc := instance.HardwareCharacteristics{
		Arch:     &spec.Image.Arch,
		Mem:      &spec.InstanceType.Mem,
		CpuCores: &spec.InstanceType.CpuCores,
		CpuPower: spec.InstanceType.CpuPower,
		RootDisk: &disk64,
	}

	return inst, &hc, nil, nil
}

func (env *joyentEnviron) AllInstances() ([]instance.Instance, error) {
	instances := []instance.Instance{}

	filter := cloudapi.NewFilter()
	filter.Set("tag.group", "juju")
	filter.Set("tag.env", env.Config().Name())

	machines, err := env.compute.cloudapi.ListMachines(filter)
	if err != nil {
		return nil, errors.Annotate(err, "cannot retrieve instances")
	}

	for _, m := range machines {
		if strings.EqualFold(m.State, "provisioning") || strings.EqualFold(m.State, "running") {
			copy := m
			instances = append(instances, &joyentInstance{machine: &copy, env: env})
		}
	}

	return instances, nil
}

func (env *joyentEnviron) Instances(ids []instance.Id) ([]instance.Instance, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	logger.Debugf("Looking for instances %q", ids)

	instances := make([]instance.Instance, len(ids))
	found := 0

	allInstances, err := env.AllInstances()
	if err != nil {
		return nil, err
	}

	for i, id := range ids {
		for _, instance := range allInstances {
			if instance.Id() == id {
				instances[i] = instance
				found++
			}
		}
	}

	logger.Debugf("Found %d instances %q", found, instances)

	if found == 0 {
		return nil, environs.ErrNoInstances
	} else if found < len(ids) {
		return instances, environs.ErrPartialInstances
	}

	return instances, nil
}

// AllocateAddress requests a new address to be allocated for the
// given instance on the given network. This is not implemented on the
// Joyent provider yet.
func (*joyentEnviron) AllocateAddress(_ instance.Id, _ network.Id) (network.Address, error) {
	return network.Address{}, errors.NotImplementedf("AllocateAddress")
}

// ListNetworks returns basic information about all networks known by
// the provider for the environment. They may be unknown to juju yet
// (i.e. when called initially or when a new network was created).
// This is not implemented on the Joyent provider yet.
func (*joyentEnviron) ListNetworks() ([]network.BasicInfo, error) {
	return nil, errors.NotImplementedf("ListNetworks")
}

func (env *joyentEnviron) StopInstances(ids ...instance.Id) error {
	// Remove all the instances in parallel so that we incur less round-trips.
	var wg sync.WaitGroup
	//var err error
	wg.Add(len(ids))
	errc := make(chan error, len(ids))
	for _, id := range ids {
		id := id // copy to new free var for closure
		go func() {
			defer wg.Done()
			if err := env.stopInstance(string(id)); err != nil {
				errc <- err
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errc:
		return errors.Annotate(err, "cannot stop all instances")
	default:
	}
	return common.RemoveStateInstances(env.Storage(), ids...)
}

func (env *joyentEnviron) stopInstance(id string) error {
	// wait for machine to be running
	// if machine is still provisioning stop will fail
	for !env.pollMachineState(id, "running") {
		time.Sleep(1 * time.Second)
	}

	err := env.compute.cloudapi.StopMachine(id)
	if err != nil {
		return errors.Annotatef(err, "cannot stop instance %v", id)
	}

	// wait for machine to be stopped
	for !env.pollMachineState(id, "stopped") {
		time.Sleep(1 * time.Second)
	}

	err = env.compute.cloudapi.DeleteMachine(id)
	if err != nil {
		return errors.Annotatef(err, "cannot delete instance %v", id)
	}

	return nil
}

func (env *joyentEnviron) pollMachineState(machineId, state string) bool {
	machineConfig, err := env.compute.cloudapi.GetMachine(machineId)
	if err != nil {
		return false
	}
	return strings.EqualFold(machineConfig.State, state)
}

func (env *joyentEnviron) listInstanceTypes() ([]instances.InstanceType, error) {
	packages, err := env.compute.cloudapi.ListPackages(nil)
	if err != nil {
		return nil, err
	}
	allInstanceTypes := []instances.InstanceType{}
	for _, pkg := range packages {
		instanceType := instances.InstanceType{
			Id:       pkg.Id,
			Name:     pkg.Name,
			Arches:   []string{arch.AMD64},
			Mem:      uint64(pkg.Memory),
			CpuCores: uint64(pkg.VCPUs),
			RootDisk: uint64(pkg.Disk * 1024),
			VirtType: &vTypeVirtualmachine,
		}
		allInstanceTypes = append(allInstanceTypes, instanceType)
	}
	return allInstanceTypes, nil
}

// FindInstanceSpec returns an InstanceSpec satisfying the supplied instanceConstraint.
func (env *joyentEnviron) FindInstanceSpec(ic *instances.InstanceConstraint) (*instances.InstanceSpec, error) {
	allInstanceTypes, err := env.listInstanceTypes()
	if err != nil {
		return nil, err
	}
	imageConstraint := imagemetadata.NewImageConstraint(simplestreams.LookupParams{
		CloudSpec: simplestreams.CloudSpec{ic.Region, env.Ecfg().SdcUrl()},
		Series:    []string{ic.Series},
		Arches:    ic.Arches,
	})
	sources, err := environs.ImageMetadataSources(env)
	if err != nil {
		return nil, err
	}

	matchingImages, _, err := imagemetadata.Fetch(sources, imageConstraint, signedImageDataOnly)
	if err != nil {
		return nil, err
	}
	images := instances.ImageMetadataToImages(matchingImages)
	spec, err := instances.FindInstanceSpec(images, ic, allInstanceTypes)
	if err != nil {
		return nil, err
	}
	return spec, nil
}
