// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package beater

import (
	"fmt"
	"github.com/elastic/beats/v7/libbeat/processors/util"
	"github.com/elastic/beats/v7/packetbeat/pb"
	"runtime"
	"sync"
	"time"

	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/cfgfile"
	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/libbeat/logp"
	"github.com/elastic/beats/v7/libbeat/publisher/pipeline"

	"github.com/elastic/beats/v7/packetbeat/config"
	"github.com/elastic/beats/v7/packetbeat/flows"
	"github.com/elastic/beats/v7/packetbeat/procs"
	"github.com/elastic/beats/v7/packetbeat/protos"
	"github.com/elastic/beats/v7/packetbeat/publish"
	"github.com/elastic/beats/v7/packetbeat/sniffer"
)



type processor struct {
	wg              sync.WaitGroup
	publisher       *publish.TransactionPublisher
	flows           *flows.Flows
	sniffer         *sniffer.Sniffer
	report 			*Reporter
	shutdownTimeout time.Duration
	err             chan error
}

func newProcessor(shutdownTimeout time.Duration, publisher *publish.TransactionPublisher, flows *flows.Flows, sniffer *sniffer.Sniffer, report *Reporter, err chan error) *processor {
	return &processor{
		publisher:       publisher,
		flows:           flows,
		sniffer:         sniffer,
		err:             err,
		shutdownTimeout: shutdownTimeout,
		report: report,
	}
}

func (p *processor) String() string {
	return "packetbeat.processor"
}

func (p *processor) Start() {
	if p.flows != nil {
		p.flows.Start()
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		err := p.sniffer.Run()
		if err != nil {
			p.err <- fmt.Errorf("sniffer loop failed: %v", err)
			return
		}
		p.err <- nil
	}()
	p.report.Start()
	//go p.sniffer.Check()
}

func (p *processor) Stop() {
	p.sniffer.Stop()
	if p.flows != nil {
		p.flows.Stop()
	}
	p.wg.Wait()
	// wait for shutdownTimeout to let the publisher flush
	// whatever pending events
	if p.shutdownTimeout > 0 {
		time.Sleep(p.shutdownTimeout)
	}
	p.publisher.Stop()
	p.report.Stop()
}

type processorFactory struct {
	name         string
	err          chan error
	beat         *beat.Beat
	configurator func(*common.Config) (config.Config, error)
}

func newProcessorFactory(name string, err chan error, beat *beat.Beat, configurator func(*common.Config) (config.Config, error)) *processorFactory {
	return &processorFactory{
		name:         name,
		err:          err,
		beat:         beat,
		configurator: configurator,
	}
}

func (p *processorFactory) Create(pipeline beat.PipelineConnector, cfg *common.Config) (cfgfile.Runner, error) {
	config, err := p.configurator(cfg)
	if err != nil {
		logp.Err("Failed to read the beat config: %v, %v", err, config)
		return nil, err
	}

	publisher, err := publish.NewTransactionPublisher(
		p.beat.Info.Name,
		p.beat.Publisher,
		config.IgnoreOutgoing,
		config.Interfaces.File == "",
		config.Interfaces.InternalNetworks,
	)
	if err != nil {
		return nil, err
	}

	watcher := procs.ProcessesWatcher{}
	// Enable the process watcher only if capturing live traffic
	if config.Interfaces.File == "" {
		err = watcher.Init(config.Procs)
		if err != nil {
			logp.Critical(err.Error())
			return nil, err
		}
	} else {
		logp.Info("Process watcher disabled when file input is used")
	}

	logp.Debug("main", "Initializing protocol plugins")
	protocols := protos.NewProtocols()
	err = protocols.Init(false, publisher, watcher, config.Protocols, config.ProtocolsList)
	if err != nil {
		return nil, fmt.Errorf("Initializing protocol analyzers failed: %v", err)
	}
	flows, err := setupFlows(pipeline, watcher, config)
	if err != nil {
		return nil, err
	}

	report, err := createReporter(config, publisher)
	if err != nil {
		return nil, err
	}

	sniffer, err := setupSniffer(config, protocols, workerFactory(publisher, protocols, watcher, flows, config))
	if err != nil {
		return nil, err
	}

	return newProcessor(config.ShutdownTimeout, publisher, flows, sniffer, report, p.err), nil
}

func (p *processorFactory) CheckConfig(config *common.Config) error {
	runner, err := p.Create(pipeline.NewNilPipeline(), config)
	if err != nil {
		return err
	}
	runner.Stop()
	return nil
}

type Reporter struct {
	//add by ytl
	reportTemplate int
	reportTopic    string
	startTime      time.Time
	results 	   protos.Reporter
	done      	   chan struct{}
	ticker         *time.Ticker
}

func (report *Reporter) publishInfo() {
	evt, _ := pb.NewBeatEvent(time.Now())
	fields := evt.Fields
	fields["type"] = "report"
	fields["topic"] = report.reportTopic
	ipList, hwList, err := util.GetEffectiveNetInfo()
	if err == nil {
		if len(ipList) != 0 {
			fields["Ip"] = ipList[0]
		}
		if len(hwList) != 0 {
			fields["Mac"] = hwList[0]
		}
	}

	fields["OS"] = runtime.GOOS
	duration := time.Now().Sub(report.startTime)
	fields["RunTime"] = duration.Seconds()
	fields["Template"] = report.reportTemplate
	report.results(evt)

}

func (report *Reporter)Start()  {
	go func(p *Reporter) {
		for {
			select {
			case <-p.done:
				return
			case <-p.ticker.C:
				report.publishInfo()
			}
		}
	}(report)
}

func (report *Reporter)Stop()  {
	close(report.done)
}

func createReporter(cfg config.Config, publisher *publish.TransactionPublisher) (*Reporter, error) {
	report := Reporter{
		reportTemplate: cfg.Report.Template,
		reportTopic:    cfg.Report.Topic,
		startTime:      time.Now(),
		done: make(chan struct{}),
		ticker : time.NewTicker(time.Second * 5),
	}

	config := common.NewConfig()
	result, err := publisher.CreateReporter(config)
	if err != nil {
		return nil, err
	}
	report.results = result
	return &report, nil
}
