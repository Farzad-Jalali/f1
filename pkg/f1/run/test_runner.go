package run

import (
	"fmt"
	"io"
	stdlog "log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"text/template"
	"time"

	"github.com/form3tech-oss/f1/pkg/f1/raterunner"

	"github.com/form3tech-oss/f1/pkg/f1/options"

	"github.com/pkg/errors"

	"github.com/form3tech-oss/f1/pkg/f1/logging"

	"github.com/form3tech-oss/f1/pkg/f1/trigger/api"

	"github.com/aholic/ggtimer"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	log "github.com/sirupsen/logrus"

	"github.com/form3tech-oss/f1/pkg/f1/metrics"
	"github.com/form3tech-oss/f1/pkg/f1/testing"
)

func NewRun(options options.RunOptions, t *api.Trigger) (*Run, error) {
	run := Run{
		Options:         options,
		RateDescription: t.Description,
		trigger:         t,
	}
	prometheusUrl := os.Getenv("PROMETHEUS_PUSH_GATEWAY")
	if prometheusUrl != "" {
		run.pusher = push.New(prometheusUrl, "f1-"+options.Scenario).Gatherer(prometheus.DefaultGatherer)
	}
	if run.Options.RegisterLogHookFunc == nil {
		run.Options.RegisterLogHookFunc = logging.NoneRegisterLogHookFunc
	}
	run.result.IgnoreDropped = options.IgnoreDropped

	progressRunner, _ := raterunner.New(func(rate time.Duration, t time.Time) {
		run.gatherProgressMetrics(rate)
		fmt.Println(run.result.Progress())
	}, []raterunner.Rate{
		{Start: time.Nanosecond, Rate: time.Second},
		{Start: time.Minute, Rate: time.Second * 10},
		{Start: time.Minute * 5, Rate: time.Minute / 2},
		{Start: time.Minute * 10, Rate: time.Minute},
	})
	run.progressRunner = progressRunner

	return &run, nil
}

type Run struct {
	Options         options.RunOptions
	busyWorkers     int32
	iteration       int32
	failures        int32
	result          RunResult
	activeScenario  *testing.ActiveScenario
	interrupt       chan os.Signal
	trigger         *api.Trigger
	RateDescription string
	pusher          *push.Pusher
	notifyDropped   sync.Once
	progressRunner  *raterunner.RateRunner
}

var startTemplate = template.Must(template.New("result parse").
	Funcs(templateFunctions).
	Parse(`{u}{bold}{intensive_blue}F1 Load Tester{-}
Running {yellow}{{.Options.Scenario}}{-} scenario for {{if .Options.MaxIterations}}up to {{.Options.MaxIterations}} iterations or up to {{end}}{{duration .Options.MaxDuration}} at a rate of {{.RateDescription}}.
`))

func (r *Run) Do() *RunResult {
	fmt.Print(renderTemplate(startTemplate, r))
	defer r.printSummary()
	defer r.printLogOnFailure()

	r.configureLogging()

	metrics.Instance().Reset()
	var err error
	r.activeScenario, err = testing.NewActiveScenarios(r.Options.Scenario, r.Options.Env, testing.GetScenario(r.Options.Scenario), 0)
	r.pushMetrics()
	fmt.Println(r.result.Setup())

	if err != nil {
		return r.fail(err, "setup failed")
	}

	// set initial started timestamp so that the progress trackers work
	r.result.RecordStarted()

	r.progressRunner.Run()
	metricsTick := ggtimer.NewTicker(5*time.Second, func(t time.Time) {
		r.pushMetrics()
	})

	r.run()

	r.progressRunner.Terminate()
	metricsTick.Close()
	r.gatherMetrics()

	r.teardown()
	r.pushMetrics()
	fmt.Println(r.result.Teardown())

	return &r.result
}

func (r *Run) configureLogging() {
	r.Options.RegisterLogHookFunc(r.Options.Scenario)
	if !r.Options.Verbose {
		r.result.LogFile = redirectLoggingToFile(r.Options.Scenario)
		welcomeMessage := renderTemplate(startTemplate, r)
		log.Info(welcomeMessage)
		fmt.Printf("Saving logs to %s\n\n", r.result.LogFile)
	}
}

func (r *Run) teardown() {
	if r.activeScenario.AutoTeardown() != nil {
		r.activeScenario.AutoTeardown().Cancel()
	}

	if r.activeScenario.TeardownFn != nil {
		err := r.activeScenario.Run(metrics.TeardownResult, "teardown", "0", "teardown", r.activeScenario.TeardownFn)
		if err != nil {
			r.fail(err, "teardown failed")
		}
	} else {
		log.Infof("nil teardown function for scenario %s", r.Options.Scenario)
	}
}

func (r *Run) printSummary() {
	summary := r.result.String()
	fmt.Println(summary)
	if !r.Options.Verbose {
		log.Info(summary)
		log.StandardLogger().SetOutput(os.Stdout)
		stdlog.SetOutput(os.Stdout)
	}
}

func (r *Run) run() {
	// handle ctrl-c interrupts
	r.interrupt = make(chan os.Signal, 1)
	signal.Notify(r.interrupt, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(r.interrupt)
	defer close(r.interrupt)

	// Build a worker group to process the iterations.
	workers := r.Options.Concurrency
	doWorkChannel := make(chan int32, workers)
	stopWorkers := make(chan struct{})

	wg := &sync.WaitGroup{}
	defer wg.Wait()

	r.busyWorkers = int32(0)
	workDone := make(chan bool, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go r.runWorker(doWorkChannel, stopWorkers, wg, fmt.Sprint(i), workDone)
	}

	// if the trigger has a limited duration, restrict the run to that duration.
	duration := r.Options.MaxDuration
	if r.trigger.Duration > 0 && r.trigger.Duration < r.Options.MaxDuration {
		duration = r.trigger.Duration
	}

	// Cancel work slightly before end of duration to avoid starting a new iteration
	durationElapsed := testing.NewCancellableTimer(duration - 5*time.Millisecond)
	r.result.RecordStarted()
	defer r.result.RecordTestFinished()

	doWork := make(chan bool, workers)
	stopTrigger := make(chan bool, 1)
	go r.trigger.Trigger(doWork, stopTrigger, workDone, r.Options)

	// run more iterations on every tick, until duration has elapsed.
	for {
		select {
		case <-r.interrupt:
			fmt.Println(r.result.Interrupted())
			r.progressRunner.RestartRate()
			// stop listening to interrupts - second interrupt will terminate immediately
			signal.Stop(r.interrupt)
			durationElapsed.Cancel()
		case <-durationElapsed.C:
			fmt.Println(r.result.MaxDurationElapsed())
			log.Info("Stopping worker")
			stopTrigger <- true
			close(stopWorkers)
			wg.Wait()
			return
		case <-doWork:
			r.doWork(doWorkChannel, durationElapsed)
		}
	}
}

func (r *Run) doWork(doWorkChannel chan<- int32, durationElapsed *testing.CancellableTimer) {
	if atomic.LoadInt32(&r.busyWorkers) >= int32(r.Options.Concurrency) {
		r.activeScenario.RecordDroppedIteration()
		r.notifyDropped.Do(func() {
			// only log once.
			log.Warn("Dropping requests as workers are too busy. Considering increasing `--concurrency` argument")
		})
		return
	}
	iteration := atomic.AddInt32(&r.iteration, 1)
	if r.Options.MaxIterations > 0 && iteration == r.Options.MaxIterations {
		doWorkChannel <- iteration
		durationElapsed.Cancel()
	} else if r.Options.MaxIterations <= 0 || iteration < r.Options.MaxIterations {
		doWorkChannel <- iteration
	}
}

func (r *Run) gatherMetrics() {
	m, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		r.result.AddError(errors.Wrap(err, "unable to gather metrics"))
	}
	for _, metric := range m {
		if *metric.Name == "form3_loadtest_iteration" {
			for _, m := range metric.Metric {
				result := "unknown"
				stage := "single"
				for _, label := range m.Label {
					if *label.Name == "result" {
						result = *label.Value
					}
					if *label.Name == "stage" {
						stage = *label.Value
					}
				}
				r.result.SetMetrics(result, stage, *m.Summary.SampleCount, m.Summary.Quantile)
			}
		}
	}
}
func (r *Run) gatherProgressMetrics(duration time.Duration) {
	m, err := metrics.Instance().ProgressRegistry.Gather()
	if err != nil {
		r.result.AddError(errors.Wrap(err, "unable to gather metrics"))
	}
	metrics.Instance().Progress.Reset()
	r.result.ClearProgressMetrics()
	for _, metric := range m {
		if *metric.Name == "form3_loadtest_iteration" {
			for _, m := range metric.Metric {
				result := "unknown"
				stage := "single"
				for _, label := range m.Label {
					if *label.Name == "result" {
						result = *label.Value
					}
					if *label.Name == "stage" {
						stage = *label.Value
					}
				}
				r.result.IncrementMetrics(duration, result, stage, *m.Summary.SampleCount, m.Summary.Quantile)
			}
		}
	}
}

func (r *Run) runWorker(input <-chan int32, stop <-chan struct{}, wg *sync.WaitGroup, worker string, workDone chan<- bool) {
	defer wg.Done()
	for {
		select {
		case <-stop:
			return
		case iteration := <-input:
			// if both stop chan is closed and input ch has more iterations,
			// select will choose a random case. double check if we need to stop
			select {
			case <-stop:
				return
			default:
			}
			atomic.AddInt32(&r.busyWorkers, 1)
			for _, stage := range r.activeScenario.Stages {
				err := r.activeScenario.Run(metrics.IterationResult, stage.Name, worker, fmt.Sprint(iteration), stage.RunFn)
				if err != nil {
					log.WithError(err).Error("failed iteration run")
					atomic.AddInt32(&r.failures, 1)
				}
			}
			atomic.AddInt32(&r.busyWorkers, -1)

			// if we need to stop - no one is listening for workDone,
			// so it will block forever. check the stop channel to stop the worker
			select {
			case workDone <- true:
			case <-stop:
				return
			}
		}
	}
}

func (r *Run) fail(err error, message string) *RunResult {
	r.result.AddError(errors.Wrap(err, message))
	return &r.result
}

func (r *Run) pushMetrics() {
	if r.pusher == nil {
		return
	}
	err := r.pusher.Push()
	if err != nil {
		log.Errorf("unable to push metrics to prometheus: %v", err)
	}
}

func (r *Run) printLogOnFailure() {
	if r.Options.Verbose || !r.Options.VerboseFail || !r.result.Failed() {
		return
	}

	if err := r.printResultLogs(); err != nil {
		log.WithError(err).Error("error printing logs")
	}
}

func (r *Run) printResultLogs() error {
	fd, err := os.Open(r.result.LogFile)
	if err != nil {
		return errors.Wrap(err, "error opening log file")
	}
	defer func() {
		if fd == nil {
			return
		}
		if err := fd.Close(); err != nil {
			log.WithError(err).Error("error closing log file")
		}
	}()

	if fd != nil {
		if _, err := io.Copy(os.Stdout, fd); err != nil {
			return errors.Wrap(err, "error printing printing logs")
		}
	}

	return nil
}