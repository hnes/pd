// Copyright 2023 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,g
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controller

import (
	"context"
	"encoding/json"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	rmpb "github.com/pingcap/kvproto/pkg/resource_manager"
	"github.com/pingcap/log"
	pd "github.com/tikv/pd/client"
	"github.com/tikv/pd/client/errs"
	"go.uber.org/zap"
)

const (
	requestUnitConfigPath  = "resource_group/ru_config"
	defaultMaxWaitDuration = time.Second
	maxRetry               = 3
	maxNotificationChanLen = 200
)

// ResourceGroupKVInterceptor is used as quota limit controller for resource group using kv store.
type ResourceGroupKVInterceptor interface {
	// OnRequestWait is used to check whether resource group has enough tokens. It maybe needs to wait some time.
	OnRequestWait(ctx context.Context, resourceGroupName string, info RequestInfo) (*rmpb.Consumption, error)
	// OnResponse is used to consume tokens after receiving response
	OnResponse(resourceGroupName string, req RequestInfo, resp ResponseInfo) (*rmpb.Consumption, error)
}

// ResourceGroupProvider provides some api to interact with resource manager server。
type ResourceGroupProvider interface {
	ListResourceGroups(ctx context.Context) ([]*rmpb.ResourceGroup, error)
	GetResourceGroup(ctx context.Context, resourceGroupName string) (*rmpb.ResourceGroup, error)
	AddResourceGroup(ctx context.Context, metaGroup *rmpb.ResourceGroup) (string, error)
	ModifyResourceGroup(ctx context.Context, metaGroup *rmpb.ResourceGroup) (string, error)
	DeleteResourceGroup(ctx context.Context, resourceGroupName string) (string, error)
	AcquireTokenBuckets(ctx context.Context, request *rmpb.TokenBucketsRequest) ([]*rmpb.TokenBucketResponse, error)
	LoadGlobalConfig(ctx context.Context, names []string, configPath string) ([]pd.GlobalConfigItem, int64, error)
}

// ResourceControlCreateOption create a ResourceGroupsController with the optional settings.
type ResourceControlCreateOption func(controller *ResourceGroupsController)

// EnableSingleGroupByKeyspace is the option to enable single group by keyspace feature.
func EnableSingleGroupByKeyspace() ResourceControlCreateOption {
	return func(controller *ResourceGroupsController) {
		controller.config.isSingleGroupByKeyspace = true
	}
}

var _ ResourceGroupKVInterceptor = (*ResourceGroupsController)(nil)

// ResourceGroupsController impls ResourceGroupKVInterceptor.
type ResourceGroupsController struct {
	clientUniqueID   uint64
	provider         ResourceGroupProvider
	groupsController sync.Map
	config           *Config

	loopCtx    context.Context
	loopCancel func()

	calculators []ResourceCalculator

	// When a signal is received, it means the number of available token is low.
	lowTokenNotifyChan chan struct{}
	// When a token bucket response received from server, it will be sent to the channel.
	tokenResponseChan chan []*rmpb.TokenBucketResponse
	// When the token bucket of a resource group is updated, it will be sent to the channel.
	tokenBucketUpdateChan chan *groupCostController

	run struct {
		now                  time.Time
		lastRequestTimeLowRU time.Time
		lastRequestTime      time.Time

		// requestInProgress is true if we are in the process of sending a request.
		// It gets set to false when we receive the response in the main loop,
		// even in error cases.
		requestInProgress bool

		// requestNeedsRetry is set if the last token bucket request encountered an
		// error. This triggers a retry attempt on the next tick.
		//
		// Note: requestNeedsRetry and requestInProgress are never true at the same time.
		requestNeedsRetry bool
	}
}

// NewResourceGroupController returns a new ResourceGroupsController which impls ResourceGroupKVInterceptor
func NewResourceGroupController(
	ctx context.Context,
	clientUniqueID uint64,
	provider ResourceGroupProvider,
	requestUnitConfig *RequestUnitConfig,
	opts ...ResourceControlCreateOption,
) (*ResourceGroupsController, error) {
	if requestUnitConfig == nil {
		var err error
		requestUnitConfig, err = loadRequestUnitConfig(ctx, provider)
		if err != nil {
			return nil, err
		}
	}
	config := GenerateConfig(requestUnitConfig)
	controller := &ResourceGroupsController{
		clientUniqueID:        clientUniqueID,
		provider:              provider,
		config:                config,
		lowTokenNotifyChan:    make(chan struct{}, 1),
		tokenResponseChan:     make(chan []*rmpb.TokenBucketResponse, 1),
		tokenBucketUpdateChan: make(chan *groupCostController, maxNotificationChanLen),
	}
	for _, opt := range opts {
		opt(controller)
	}
	controller.calculators = []ResourceCalculator{newKVCalculator(controller.config), newSQLCalculator(controller.config)}
	return controller, nil
}

func loadRequestUnitConfig(ctx context.Context, provider ResourceGroupProvider) (*RequestUnitConfig, error) {
	items, _, err := provider.LoadGlobalConfig(ctx, nil, requestUnitConfigPath)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, errors.Errorf("failed to load the ru config from remote server")
	}
	ruConfig := &RequestUnitConfig{}
	err = json.Unmarshal(items[0].PayLoad, ruConfig)
	if err != nil {
		return nil, err
	}
	return ruConfig, nil
}

// GetConfig returns the config of controller. It's only used for test.
func (c *ResourceGroupsController) GetConfig() *Config {
	return c.config
}

// Source List
const (
	FromPeriodReport = "period_report"
	FromLowRU        = "low_ru"
)

// Start starts ResourceGroupController service.
func (c *ResourceGroupsController) Start(ctx context.Context) {
	c.initRunState()
	c.loopCtx, c.loopCancel = context.WithCancel(ctx)
	go func() {
		cleanupTicker := time.NewTicker(defaultGroupCleanupInterval)
		defer cleanupTicker.Stop()
		stateUpdateTicker := time.NewTicker(defaultGroupStateUpdateInterval)
		defer stateUpdateTicker.Stop()

		failpoint.Inject("fastCleanup", func() {
			cleanupTicker.Stop()
			cleanupTicker = time.NewTicker(100 * time.Millisecond)
		})

		for {
			select {
			case <-c.loopCtx.Done():
				return
			case resp := <-c.tokenResponseChan:
				c.run.requestInProgress = false
				if resp != nil {
					c.updateRunState()
					c.handleTokenBucketResponse(resp)
				} else {
					// A nil response indicates a failure (which would have been logged).
					c.run.requestNeedsRetry = true
				}
			case <-cleanupTicker.C:
				if err := c.cleanUpResourceGroup(c.loopCtx); err != nil {
					log.Error("[resource group controller] clean up resource groups failed", zap.Error(err))
				}
			case <-stateUpdateTicker.C:
				c.updateRunState()
				c.updateAvgRequestResourcePerSec()
				if c.run.requestNeedsRetry || c.shouldReportConsumption() {
					c.run.requestNeedsRetry = false
					c.collectTokenBucketRequests(c.loopCtx, FromPeriodReport, false /* select all */)
				}
			case <-c.lowTokenNotifyChan:
				c.updateRunState()
				c.updateAvgRequestResourcePerSec()
				if !c.run.requestInProgress {
					c.collectTokenBucketRequests(c.loopCtx, FromLowRU, true /* only select low tokens resource group */)
				}
			case gc := <-c.tokenBucketUpdateChan:
				now := gc.run.now
				go gc.handleTokenBucketUpdateEvent(c.loopCtx, now)
			}
		}
	}()
}

// Stop stops ResourceGroupController service.
func (c *ResourceGroupsController) Stop() error {
	if c.loopCancel == nil {
		return errors.Errorf("resource groups controller does not start")
	}
	c.loopCancel()
	return nil
}

// tryGetResourceGroup will try to get the resource group controller from local cache first,
// if the local cache misses, it will then call gRPC to fetch the resource group info from server.
func (c *ResourceGroupsController) tryGetResourceGroup(ctx context.Context, name string) (*groupCostController, error) {
	// Get from the local cache first.
	if tmp, ok := c.groupsController.Load(name); ok {
		return tmp.(*groupCostController), nil
	}
	// Call gRPC to fetch the resource group info.
	group, err := c.provider.GetResourceGroup(ctx, name)
	if err != nil {
		return nil, err
	}
	// Check again to prevent initializing the same resource group concurrently.
	if tmp, ok := c.groupsController.Load(name); ok {
		gc := tmp.(*groupCostController)
		return gc, nil
	}
	// Initialize the resource group controller.
	gc, err := newGroupCostController(group, c.config, c.lowTokenNotifyChan, c.tokenBucketUpdateChan)
	if err != nil {
		return nil, err
	}
	// TODO: re-init the state if user change mode from RU to RAW mode.
	gc.initRunState()
	// Check again to prevent initializing the same resource group concurrently.
	tmp, loaded := c.groupsController.LoadOrStore(group.GetName(), gc)
	if !loaded {
		log.Info("[resource group controller] create resource group cost controller", zap.String("name", group.GetName()))
	}
	return tmp.(*groupCostController), nil
}

func (c *ResourceGroupsController) cleanUpResourceGroup(ctx context.Context) error {
	groups, err := c.provider.ListResourceGroups(ctx)
	if err != nil {
		return errs.ErrClientListResourceGroup.FastGenByArgs(err.Error())
	}
	latestGroups := make(map[string]struct{})
	for _, group := range groups {
		latestGroups[group.GetName()] = struct{}{}
	}
	c.groupsController.Range(func(key, value any) bool {
		resourceGroupName := key.(string)
		if _, ok := latestGroups[resourceGroupName]; !ok {
			c.groupsController.Delete(key)
			return true
		}

		gc := value.(*groupCostController)
		// Check for stale resource groups, which will be deleted when consumption is continuously unchanged.
		gc.mu.Lock()
		latestConsumption := *gc.mu.consumption
		gc.mu.Unlock()
		if equalRU(latestConsumption, *gc.run.consumption) {
			if gc.tombstone {
				c.groupsController.Delete(resourceGroupName)
				return true
			}
			gc.tombstone = true
		} else {
			gc.tombstone = false
		}
		return true
	})
	return nil
}

func (c *ResourceGroupsController) initRunState() {
	now := time.Now()
	c.run.now = now
	c.run.lastRequestTime = now
	c.run.lastRequestTimeLowRU = now
}

func (c *ResourceGroupsController) updateRunState() {
	c.run.now = time.Now()
	c.groupsController.Range(func(name, value any) bool {
		gc := value.(*groupCostController)
		gc.updateRunState()
		return true
	})
}

func (c *ResourceGroupsController) shouldReportConsumption() bool {
	if c.run.requestInProgress {
		return false
	}
	maxDuration := func(a, b time.Duration) time.Duration {
		if a > b {
			return a
		}
		return b
	}
	timeSinceLastRequest := maxDuration(c.run.now.Sub(c.run.lastRequestTime), c.run.now.Sub(c.run.lastRequestTimeLowRU))
	if timeSinceLastRequest >= defaultTargetPeriod {
		if timeSinceLastRequest >= extendedReportingPeriodFactor*defaultTargetPeriod {
			return true
		}
		ret := false
		c.groupsController.Range(func(name, value any) bool {
			gc := value.(*groupCostController)
			ret = ret || gc.shouldReportConsumption()
			return !ret
		})
		return ret
	}
	return false
}

func (c *ResourceGroupsController) updateAvgRequestResourcePerSec() {
	c.groupsController.Range(func(name, value any) bool {
		gc := value.(*groupCostController)
		gc.updateAvgRequestResourcePerSec()
		return true
	})
}

func (c *ResourceGroupsController) handleTokenBucketResponse(resp []*rmpb.TokenBucketResponse) {
	for _, res := range resp {
		name := res.GetResourceGroupName()
		v, ok := c.groupsController.Load(name)
		if !ok {
			log.Warn("[resource group controller] a non-existent resource group was found when handle token response", zap.String("name", name))
			continue
		}
		gc := v.(*groupCostController)
		gc.handleTokenBucketResponse(res)
	}
}

func (c *ResourceGroupsController) collectTokenBucketRequests(ctx context.Context, source string, onlySelectLow bool) {
	requests := make([]*rmpb.TokenBucketRequest, 0)
	c.groupsController.Range(func(name, value any) bool {
		gc := value.(*groupCostController)
		request := gc.collectRequestAndConsumption(onlySelectLow)
		if request != nil {
			requests = append(requests, request)
		}
		return true
	})
	if len(requests) > 0 {
		c.sendTokenBucketRequests(ctx, requests, source)
	}
}

func (c *ResourceGroupsController) sendTokenBucketRequests(ctx context.Context, requests []*rmpb.TokenBucketRequest, source string) {
	now := time.Now()
	switch source {
	case FromLowRU:
		c.run.lastRequestTimeLowRU = now
	case FromPeriodReport:
		c.run.lastRequestTime = now
	}
	c.run.requestInProgress = true
	req := &rmpb.TokenBucketsRequest{
		Requests:              requests,
		TargetRequestPeriodMs: uint64(defaultTargetPeriod / time.Millisecond),
	}
	go func() {
		log.Debug("[resource group controller] send token bucket request", zap.Time("now", now), zap.Any("req", req.Requests), zap.String("source", source))
		resp, err := c.provider.AcquireTokenBuckets(ctx, req)
		if err != nil {
			// Don't log any errors caused by the stopper canceling the context.
			if !errors.ErrorEqual(err, context.Canceled) {
				log.L().Sugar().Infof("[resource group controller] token bucket rpc error: %v", err)
			}
			resp = nil
		}
		log.Debug("[resource group controller] token bucket response", zap.Time("now", time.Now()), zap.Any("resp", resp), zap.String("source", source), zap.Duration("latency", time.Since(now)))
		c.tokenResponseChan <- resp
	}()
}

// OnRequestWait is used to check whether resource group has enough tokens. It maybe needs wait some time.
func (c *ResourceGroupsController) OnRequestWait(
	ctx context.Context, resourceGroupName string, info RequestInfo,
) (*rmpb.Consumption, error) {
	gc, err := c.tryGetResourceGroup(ctx, resourceGroupName)
	if err != nil {
		return nil, err
	}
	return gc.onRequestWait(ctx, info)
}

// OnResponse is used to consume tokens after receiving response
func (c *ResourceGroupsController) OnResponse(
	resourceGroupName string, req RequestInfo, resp ResponseInfo,
) (*rmpb.Consumption, error) {
	tmp, ok := c.groupsController.Load(resourceGroupName)
	if !ok {
		log.Warn("[resource group controller] resource group name does not exist", zap.String("resourceGroupName", resourceGroupName))
		return &rmpb.Consumption{}, nil
	}
	return tmp.(*groupCostController).onResponse(req, resp)
}

type groupCostController struct {
	*rmpb.ResourceGroup
	mainCfg     *Config
	calculators []ResourceCalculator
	mode        rmpb.GroupMode

	handleRespFunc func(*rmpb.TokenBucketResponse)

	mu struct {
		sync.Mutex
		consumption *rmpb.Consumption
	}

	// fast path to make once token limit with un-limit burst.
	burstable *atomic.Bool

	lowRUNotifyChan       chan<- struct{}
	tokenBucketUpdateChan chan<- *groupCostController

	// run contains the state that is updated by the main loop.
	run struct {
		now time.Time

		// targetPeriod stores the value of the TargetPeriodSetting setting at the
		// last update.
		targetPeriod time.Duration

		// consumptions stores the last value of mu.consumption.
		// requestUnitConsumptions []*rmpb.RequestUnitItem
		// resourceConsumptions    []*rmpb.ResourceItem
		consumption *rmpb.Consumption

		// lastRequestUnitConsumptions []*rmpb.RequestUnitItem
		// lastResourceConsumptions    []*rmpb.ResourceItem
		lastRequestConsumption *rmpb.Consumption

		// initialRequestCompleted is set to true when the first token bucket
		// request completes successfully.
		initialRequestCompleted bool

		resourceTokens    map[rmpb.RawResourceType]*tokenCounter
		requestUnitTokens map[rmpb.RequestUnitType]*tokenCounter
	}

	tombstone bool
}

type tokenCounter struct {
	// avgRUPerSec is an exponentially-weighted moving average of the RU
	// consumption per second; used to estimate the RU requirements for the next
	// request.
	avgRUPerSec float64
	// lastSecRU is the consumption.RU value when avgRUPerSec was last updated.
	avgRUPerSecLastRU float64
	avgLastTime       time.Time

	notify struct {
		mu                         sync.Mutex
		setupNotificationCh        <-chan time.Time
		setupNotificationThreshold float64
		setupNotificationTimer     *time.Timer
	}

	lastDeadline time.Time
	lastRate     float64

	limiter *Limiter
}

func newGroupCostController(
	group *rmpb.ResourceGroup,
	mainCfg *Config,
	lowRUNotifyChan chan struct{},
	tokenBucketUpdateChan chan *groupCostController,
) (*groupCostController, error) {
	switch group.Mode {
	case rmpb.GroupMode_RUMode:
		if group.RUSettings.RU == nil || group.RUSettings.RU.Settings == nil {
			return nil, errs.ErrClientResourceGroupConfigUnavailable.FastGenByArgs("not configured")
		}
	default:
		return nil, errs.ErrClientResourceGroupConfigUnavailable.FastGenByArgs("not supports the resource type")
	}

	gc := &groupCostController{
		ResourceGroup: group,
		mainCfg:       mainCfg,
		calculators: []ResourceCalculator{
			newKVCalculator(mainCfg),
			newSQLCalculator(mainCfg),
		},
		mode:                  group.GetMode(),
		tokenBucketUpdateChan: tokenBucketUpdateChan,
		lowRUNotifyChan:       lowRUNotifyChan,
		burstable:             &atomic.Bool{},
	}

	switch gc.mode {
	case rmpb.GroupMode_RUMode:
		gc.handleRespFunc = gc.handleRUTokenResponse
	case rmpb.GroupMode_RawMode:
		gc.handleRespFunc = gc.handleRawResourceTokenResponse
	}

	gc.mu.consumption = &rmpb.Consumption{}
	return gc, nil
}

func (gc *groupCostController) initRunState() {
	now := time.Now()
	gc.run.now = now
	gc.run.targetPeriod = defaultTargetPeriod
	gc.run.consumption = &rmpb.Consumption{}
	gc.run.lastRequestConsumption = &rmpb.Consumption{SqlLayerCpuTimeMs: getSQLProcessCPUTime(gc.mainCfg.isSingleGroupByKeyspace)}

	cfgFunc := func(tb *rmpb.TokenBucket) tokenBucketReconfigureArgs {
		cfg := tokenBucketReconfigureArgs{
			NewTokens: initialRequestUnits,
			NewBurst:  tb.Settings.BurstLimit,
			// This is to trigger token requests as soon as resource group start consuming tokens.
			NotifyThreshold: math.Max(initialRequestUnits-float64(tb.Settings.FillRate)*0.2, 1),
		}
		if cfg.NewBurst >= 0 {
			cfg.NewBurst = 0
		}
		return cfg
	}

	switch gc.mode {
	case rmpb.GroupMode_RUMode:
		gc.run.requestUnitTokens = make(map[rmpb.RequestUnitType]*tokenCounter)
		for typ := range requestUnitLimitTypeList {
			tb := getRUTokenBucketSetting(gc.ResourceGroup, typ)
			cfg := cfgFunc(tb)
			limiter := NewLimiterWithCfg(now, cfg, gc.lowRUNotifyChan)
			counter := &tokenCounter{
				limiter:     limiter,
				avgRUPerSec: 0,
				avgLastTime: now,
			}
			gc.run.requestUnitTokens[typ] = counter
		}
	case rmpb.GroupMode_RawMode:
		gc.run.resourceTokens = make(map[rmpb.RawResourceType]*tokenCounter)
		for typ := range requestResourceLimitTypeList {
			tb := getRawResourceTokenBucketSetting(gc.ResourceGroup, typ)
			cfg := cfgFunc(tb)
			limiter := NewLimiterWithCfg(now, cfg, gc.lowRUNotifyChan)
			counter := &tokenCounter{
				limiter:     limiter,
				avgRUPerSec: 0,
				avgLastTime: now,
			}
			gc.run.resourceTokens[typ] = counter
		}
	}
}

func (gc *groupCostController) updateRunState() {
	newTime := time.Now()
	gc.mu.Lock()
	for _, calc := range gc.calculators {
		calc.Trickle(gc.mu.consumption)
	}
	*gc.run.consumption = *gc.mu.consumption
	gc.mu.Unlock()
	log.Debug("[resource group controller] update run state", zap.Any("request unit consumption", gc.run.consumption))
	gc.run.now = newTime
}

func (gc *groupCostController) updateAvgRequestResourcePerSec() {
	switch gc.mode {
	case rmpb.GroupMode_RawMode:
		gc.updateAvgRaWResourcePerSec()
	case rmpb.GroupMode_RUMode:
		gc.updateAvgRUPerSec()
	}
}

func (gc *groupCostController) handleTokenBucketUpdateEvent(ctx context.Context, now time.Time) {
	switch gc.mode {
	case rmpb.GroupMode_RawMode:
		for _, counter := range gc.run.resourceTokens {
			counter.notify.mu.Lock()
			ch := counter.notify.setupNotificationCh
			counter.notify.mu.Unlock()
			if ch == nil {
				continue
			}
			select {
			case <-ch:
				counter.notify.mu.Lock()
				counter.notify.setupNotificationTimer = nil
				counter.notify.setupNotificationCh = nil
				threshold := counter.notify.setupNotificationThreshold
				counter.notify.mu.Unlock()
				counter.limiter.SetupNotificationThreshold(now, threshold)
			case <-ctx.Done():
				return
			}
		}

	case rmpb.GroupMode_RUMode:
		for _, counter := range gc.run.requestUnitTokens {
			counter.notify.mu.Lock()
			ch := counter.notify.setupNotificationCh
			counter.notify.mu.Unlock()
			if ch == nil {
				continue
			}
			select {
			case <-ch:
				counter.notify.mu.Lock()
				counter.notify.setupNotificationTimer = nil
				counter.notify.setupNotificationCh = nil
				threshold := counter.notify.setupNotificationThreshold
				counter.notify.mu.Unlock()
				counter.limiter.SetupNotificationThreshold(now, threshold)
			case <-ctx.Done():
				return
			}
		}
	}
}

func (gc *groupCostController) updateAvgRaWResourcePerSec() {
	isBurstable := true
	for typ, counter := range gc.run.resourceTokens {
		if counter.limiter.GetBurst() >= 0 {
			isBurstable = false
		}
		if !gc.calcAvg(counter, getRawResourceValueFromConsumption(gc.run.consumption, typ)) {
			continue
		}
		log.Debug("[resource group controller] update avg raw resource per sec", zap.String("name", gc.Name), zap.String("type", rmpb.RawResourceType_name[int32(typ)]), zap.Float64("avgRUPerSec", counter.avgRUPerSec))
	}
	gc.burstable.Store(isBurstable)
}

func (gc *groupCostController) updateAvgRUPerSec() {
	isBurstable := true
	for typ, counter := range gc.run.requestUnitTokens {
		if counter.limiter.GetBurst() >= 0 {
			isBurstable = false
		}
		if !gc.calcAvg(counter, getRUValueFromConsumption(gc.run.consumption, typ)) {
			continue
		}
		log.Debug("[resource group controller] update avg ru per sec", zap.String("name", gc.Name), zap.String("type", rmpb.RequestUnitType_name[int32(typ)]), zap.Float64("avgRUPerSec", counter.avgRUPerSec))
	}
	gc.burstable.Store(isBurstable)
}

func (gc *groupCostController) calcAvg(counter *tokenCounter, new float64) bool {
	deltaDuration := gc.run.now.Sub(counter.avgLastTime)
	if deltaDuration <= 500*time.Millisecond {
		return false
	}
	delta := (new - counter.avgRUPerSecLastRU) / deltaDuration.Seconds()
	counter.avgRUPerSec = movingAvgFactor*counter.avgRUPerSec + (1-movingAvgFactor)*delta
	counter.avgLastTime = gc.run.now
	counter.avgRUPerSecLastRU = new
	return true
}

func (gc *groupCostController) shouldReportConsumption() bool {
	switch gc.Mode {
	case rmpb.GroupMode_RUMode:
		for typ := range requestUnitLimitTypeList {
			if getRUValueFromConsumption(gc.run.consumption, typ)-getRUValueFromConsumption(gc.run.lastRequestConsumption, typ) >= consumptionsReportingThreshold {
				return true
			}
		}
	case rmpb.GroupMode_RawMode:
		for typ := range requestResourceLimitTypeList {
			if getRawResourceValueFromConsumption(gc.run.consumption, typ)-getRawResourceValueFromConsumption(gc.run.lastRequestConsumption, typ) >= consumptionsReportingThreshold {
				return true
			}
		}
	}
	return false
}

func (gc *groupCostController) handleTokenBucketResponse(resp *rmpb.TokenBucketResponse) {
	gc.handleRespFunc(resp)
	if !gc.run.initialRequestCompleted {
		gc.run.initialRequestCompleted = true
		// This is the first successful request. Take back the initial RUs that we
		// used to pre-fill the bucket.
		for _, counter := range gc.run.resourceTokens {
			counter.limiter.RemoveTokens(gc.run.now, initialRequestUnits)
		}
		for _, counter := range gc.run.requestUnitTokens {
			counter.limiter.RemoveTokens(gc.run.now, initialRequestUnits)
		}
	}
}

func (gc *groupCostController) handleRawResourceTokenResponse(resp *rmpb.TokenBucketResponse) {
	for _, grantedTB := range resp.GetGrantedResourceTokens() {
		typ := grantedTB.GetType()
		counter, ok := gc.run.resourceTokens[typ]
		if !ok {
			log.Warn("[resource group controller] not support this resource type", zap.String("type", rmpb.RawResourceType_name[int32(typ)]))
			continue
		}
		gc.modifyTokenCounter(counter, grantedTB.GetGrantedTokens(), grantedTB.GetTrickleTimeMs())
	}
}

func (gc *groupCostController) handleRUTokenResponse(resp *rmpb.TokenBucketResponse) {
	for _, grantedTB := range resp.GetGrantedRUTokens() {
		typ := grantedTB.GetType()
		counter, ok := gc.run.requestUnitTokens[typ]
		if !ok {
			log.Warn("[resource group controller] not support this resource type", zap.String("type", rmpb.RawResourceType_name[int32(typ)]))
			continue
		}
		gc.modifyTokenCounter(counter, grantedTB.GetGrantedTokens(), grantedTB.GetTrickleTimeMs())
	}
}

func (gc *groupCostController) modifyTokenCounter(counter *tokenCounter, bucket *rmpb.TokenBucket, trickleTimeMs int64) {
	granted := bucket.GetTokens()
	if !counter.lastDeadline.IsZero() {
		// If last request came with a trickle duration, we may have RUs that were
		// not made available to the bucket yet; throw them together with the newly
		// granted RUs.
		if since := counter.lastDeadline.Sub(gc.run.now); since > 0 {
			granted += counter.lastRate * since.Seconds()
		}
	}
	counter.notify.mu.Lock()
	if counter.notify.setupNotificationTimer != nil {
		counter.notify.setupNotificationTimer.Stop()
		counter.notify.setupNotificationTimer = nil
		counter.notify.setupNotificationCh = nil
	}
	counter.notify.mu.Unlock()
	notifyThreshold := granted * notifyFraction
	if notifyThreshold < bufferRUs {
		notifyThreshold = bufferRUs
	}

	var cfg tokenBucketReconfigureArgs
	cfg.NewBurst = bucket.GetSettings().GetBurstLimit()
	// when trickleTimeMs equals zero, server has enough tokens and does not need to
	// limit client consume token. So all token is granted to client right now.
	if trickleTimeMs == 0 {
		cfg.NewTokens = granted
		cfg.NewRate = float64(bucket.GetSettings().FillRate)
		cfg.NotifyThreshold = notifyThreshold
		counter.lastDeadline = time.Time{}
		// In the non-trickle case, clients can be allowed to accumulate more tokens.
		if cfg.NewBurst >= 0 {
			cfg.NewBurst = 0
		}
	} else {
		// Otherwise the granted token is delivered to the client by fill rate.
		cfg.NewTokens = 0
		trickleDuration := time.Duration(trickleTimeMs) * time.Millisecond
		deadline := gc.run.now.Add(trickleDuration)
		cfg.NewRate = float64(bucket.GetSettings().FillRate) + granted/trickleDuration.Seconds()

		timerDuration := trickleDuration - time.Second
		if timerDuration <= 0 {
			timerDuration = (trickleDuration + time.Second) / 2
		}
		counter.notify.mu.Lock()
		counter.notify.setupNotificationTimer = time.NewTimer(timerDuration)
		counter.notify.setupNotificationCh = counter.notify.setupNotificationTimer.C
		counter.notify.setupNotificationThreshold = notifyThreshold
		counter.notify.mu.Unlock()
		counter.lastDeadline = deadline
		select {
		case gc.tokenBucketUpdateChan <- gc:
		default:
		}
	}

	counter.lastRate = cfg.NewRate
	counter.limiter.Reconfigure(gc.run.now, cfg)
}

func (gc *groupCostController) collectRequestAndConsumption(onlySelectLow bool) *rmpb.TokenBucketRequest {
	req := &rmpb.TokenBucketRequest{
		ResourceGroupName: gc.ResourceGroup.GetName(),
	}
	// collect request resource
	selected := !onlySelectLow
	switch gc.mode {
	case rmpb.GroupMode_RawMode:
		requests := make([]*rmpb.RawResourceItem, 0, len(requestResourceLimitTypeList))
		for typ, counter := range gc.run.resourceTokens {
			if onlySelectLow && counter.limiter.IsLowTokens() {
				selected = true
			}
			request := &rmpb.RawResourceItem{
				Type:  typ,
				Value: gc.calcRequest(counter),
			}
			requests = append(requests, request)
		}
		req.Request = &rmpb.TokenBucketRequest_RawResourceItems{
			RawResourceItems: &rmpb.TokenBucketRequest_RequestRawResource{
				RequestRawResource: requests,
			},
		}
	case rmpb.GroupMode_RUMode:
		requests := make([]*rmpb.RequestUnitItem, 0, len(requestUnitLimitTypeList))
		for typ, counter := range gc.run.requestUnitTokens {
			if onlySelectLow && counter.limiter.IsLowTokens() {
				selected = true
			}
			request := &rmpb.RequestUnitItem{
				Type:  typ,
				Value: gc.calcRequest(counter),
			}
			requests = append(requests, request)
		}
		req.Request = &rmpb.TokenBucketRequest_RuItems{
			RuItems: &rmpb.TokenBucketRequest_RequestRU{
				RequestRU: requests,
			},
		}
	}
	if !selected {
		return nil
	}

	deltaConsumption := &rmpb.Consumption{}
	*deltaConsumption = *gc.run.consumption
	sub(deltaConsumption, gc.run.lastRequestConsumption)
	req.ConsumptionSinceLastRequest = deltaConsumption

	*gc.run.lastRequestConsumption = *gc.run.consumption
	return req
}

func (gc *groupCostController) calcRequest(counter *tokenCounter) float64 {
	value := counter.avgRUPerSec*gc.run.targetPeriod.Seconds() + bufferRUs
	value -= counter.limiter.AvailableTokens(gc.run.now)
	if value < 0 {
		value = 0
	}
	return value
}

func (gc *groupCostController) onRequestWait(
	ctx context.Context, info RequestInfo,
) (*rmpb.Consumption, error) {
	delta := &rmpb.Consumption{}
	for _, calc := range gc.calculators {
		calc.BeforeKVRequest(delta, info)
	}
	if !gc.burstable.Load() {
		var err error
		now := time.Now()
	retryLoop:
		for i := 0; i < maxRetry; i++ {
			switch gc.mode {
			case rmpb.GroupMode_RawMode:
				res := make([]*Reservation, 0, len(requestResourceLimitTypeList))
				for typ, counter := range gc.run.resourceTokens {
					if v := getRawResourceValueFromConsumption(delta, typ); v > 0 {
						res = append(res, counter.limiter.Reserve(ctx, defaultMaxWaitDuration, now, v))
					}
				}
				if err = WaitReservations(ctx, now, res); err == nil {
					break retryLoop
				}
			case rmpb.GroupMode_RUMode:
				res := make([]*Reservation, 0, len(requestUnitLimitTypeList))
				for typ, counter := range gc.run.requestUnitTokens {
					if v := getRUValueFromConsumption(delta, typ); v > 0 {
						res = append(res, counter.limiter.Reserve(ctx, defaultMaxWaitDuration, now, v))
					}
				}
				if err = WaitReservations(ctx, now, res); err == nil {
					break retryLoop
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			return nil, err
		}
	}
	gc.mu.Lock()
	add(gc.mu.consumption, delta)
	gc.mu.Unlock()
	return delta, nil
}

func (gc *groupCostController) onResponse(
	req RequestInfo, resp ResponseInfo,
) (*rmpb.Consumption, error) {
	delta := &rmpb.Consumption{}
	for _, calc := range gc.calculators {
		calc.AfterKVRequest(delta, req, resp)
	}
	if !gc.burstable.Load() {
		switch gc.mode {
		case rmpb.GroupMode_RawMode:
			for typ, counter := range gc.run.resourceTokens {
				if v := getRawResourceValueFromConsumption(delta, typ); v > 0 {
					counter.limiter.RemoveTokens(time.Now(), v)
				}
			}
		case rmpb.GroupMode_RUMode:
			for typ, counter := range gc.run.requestUnitTokens {
				if v := getRUValueFromConsumption(delta, typ); v > 0 {
					counter.limiter.RemoveTokens(time.Now(), v)
				}
			}
		}
	}
	gc.mu.Lock()
	add(gc.mu.consumption, delta)
	gc.mu.Unlock()
	return delta, nil
}

// CheckResourceGroupExist checks if groupsController map {rg.name -> resource group controller}
// contains name. Used for test only.
func (c *ResourceGroupsController) CheckResourceGroupExist(name string) bool {
	_, ok := c.groupsController.Load(name)
	return ok
}

// This is used for test only.
func (gc *groupCostController) getKVCalculator() *KVCalculator {
	for _, calc := range gc.calculators {
		if kvCalc, ok := calc.(*KVCalculator); ok {
			return kvCalc
		}
	}
	return nil
}
