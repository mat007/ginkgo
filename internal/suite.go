package internal

import (
	"fmt"
	"time"

	"github.com/onsi/ginkgo/formatter"
	"github.com/onsi/ginkgo/internal/parallel_support"
	"github.com/onsi/ginkgo/reporters"
	"github.com/onsi/ginkgo/types"
)

type Phase uint

const (
	PhaseBuildTopLevel Phase = iota
	PhaseBuildTree
	PhaseRun
)

type Suite struct {
	tree               TreeNode
	topLevelContainers Nodes

	phase Phase

	suiteNodes Nodes

	writer            WriterInterface
	currentSpecReport types.SpecReport

	client parallel_support.Client
}

func NewSuite() *Suite {
	return &Suite{
		phase: PhaseBuildTopLevel,
	}
}

func (suite *Suite) BuildTree() error {
	// During PhaseBuildTopLevel, the top level containers are stored in suite.topLevelCotainers and entered
	// We now enter PhaseBuildTree where these top level containers are entered and added to the spec tree
	suite.phase = PhaseBuildTree
	for _, topLevelContainer := range suite.topLevelContainers {
		err := suite.PushNode(topLevelContainer)
		if err != nil {
			return err
		}
	}
	return nil
}

func (suite *Suite) Run(description string, suitePath string, failer *Failer, reporter reporters.Reporter, writer WriterInterface, outputInterceptor OutputInterceptor, interruptHandler InterruptHandlerInterface, suiteConfig types.SuiteConfig) (bool, bool) {
	if suite.phase != PhaseBuildTree {
		panic("cannot run before building the tree = call suite.BuildTree() first")
	}
	tree := ApplyNestedFocusPolicyToTree(suite.tree)
	specs := GenerateSpecsFromTreeRoot(tree)
	specs = ShuffleSpecs(specs, suiteConfig)
	specs, hasProgrammaticFocus := ApplyFocusToSpecs(specs, description, suiteConfig)

	suite.phase = PhaseRun
	if suiteConfig.ParallelTotal > 1 {
		suite.client = parallel_support.NewClient(suiteConfig.ParallelHost)
	}

	success := suite.runSpecs(description, suitePath, hasProgrammaticFocus, specs, failer, reporter, writer, outputInterceptor, interruptHandler, suiteConfig)
	return success, hasProgrammaticFocus
}

/*
  Tree Construction methods

  PushNode is used during PhaseBuildTopLevel and PhaseBuildTree
*/

func (suite *Suite) PushNode(node Node) error {
	if node.NodeType.Is(types.NodeTypeBeforeSuite, types.NodeTypeAfterSuite, types.NodeTypeSynchronizedBeforeSuite, types.NodeTypeSynchronizedAfterSuite, types.NodeTypeReportAfterSuite) {
		return suite.pushSuiteNode(node)
	}

	if suite.phase == PhaseRun {
		return types.GinkgoErrors.PushingNodeInRunPhase(node.NodeType, node.CodeLocation)
	}

	if node.NodeType == types.NodeTypeContainer {
		// During PhaseBuildTopLevel we only track the top level containers without entering them
		// We only enter the top level container nodes during PhaseBuildTree
		//
		// This ensures the tree is only constructed after `go spec` has called `flag.Parse()` and gives
		// the user an opportunity to load suiteConfiguration information in the `TestX` go spec hook just before `RunSpecs`
		// is invoked.  This makes the lifecycle easier to reason about and solves issues like #693.
		if suite.phase == PhaseBuildTopLevel {
			suite.topLevelContainers = append(suite.topLevelContainers, node)
			return nil
		}
		if suite.phase == PhaseBuildTree {
			parentTree := suite.tree
			suite.tree = TreeNode{Node: node}
			err := func() (err error) {
				defer func() {
					if e := recover(); e != nil {
						err = types.GinkgoErrors.CaughtPanicDuringABuildPhase(e, node.CodeLocation)
					}
				}()
				node.Body()
				return err
			}()
			suite.tree = AppendTreeNodeChild(parentTree, suite.tree)
			return err
		}
	} else {
		suite.tree = AppendTreeNodeChild(suite.tree, TreeNode{Node: node})
		return nil
	}

	return nil
}

func (suite *Suite) pushSuiteNode(node Node) error {
	if suite.phase == PhaseBuildTree {
		return types.GinkgoErrors.SuiteNodeInNestedContext(node.NodeType, node.CodeLocation)
	}

	if suite.phase == PhaseRun {
		return types.GinkgoErrors.SuiteNodeDuringRunPhase(node.NodeType, node.CodeLocation)
	}

	switch node.NodeType {
	case types.NodeTypeBeforeSuite, types.NodeTypeSynchronizedBeforeSuite:
		existingBefores := suite.suiteNodes.WithType(types.NodeTypeBeforeSuite, types.NodeTypeSynchronizedBeforeSuite)
		if len(existingBefores) > 0 {
			return types.GinkgoErrors.MultipleBeforeSuiteNodes(node.NodeType, node.CodeLocation, existingBefores[0].NodeType, existingBefores[0].CodeLocation)
		}
	case types.NodeTypeAfterSuite, types.NodeTypeSynchronizedAfterSuite:
		existingAfters := suite.suiteNodes.WithType(types.NodeTypeAfterSuite, types.NodeTypeSynchronizedAfterSuite)
		if len(existingAfters) > 0 {
			return types.GinkgoErrors.MultipleAfterSuiteNodes(node.NodeType, node.CodeLocation, existingAfters[0].NodeType, existingAfters[0].CodeLocation)
		}
	}

	suite.suiteNodes = append(suite.suiteNodes, node)
	return nil
}

/*
  Spec Running methods - used during PhaseRun
*/
func (suite *Suite) CurrentSpecReport() types.SpecReport {
	report := suite.currentSpecReport
	if suite.writer != nil {
		report.CapturedGinkgoWriterOutput = string(suite.writer.Bytes())
	}
	return report
}

func (suite *Suite) AddReportEntry(entry ReportEntry) error {
	if suite.phase != PhaseRun {
		return types.GinkgoErrors.AddReportEntryNotDuringRunPhase(entry.Location)
	}
	suite.currentSpecReport.ReportEntries = append(suite.currentSpecReport.ReportEntries, entry)
	return nil
}

func (suite *Suite) runSpecs(description string, suitePath string, hasProgrammaticFocus bool, specs Specs, failer *Failer, reporter reporters.Reporter, writer WriterInterface, outputInterceptor OutputInterceptor, interruptHandler InterruptHandlerInterface, suiteConfig types.SuiteConfig) bool {
	suite.writer = writer

	numSpecsThatWillBeRun := specs.CountWithoutSkip()

	report := types.Report{
		SuitePath:                 suitePath,
		SuiteDescription:          description,
		SuiteConfig:               suiteConfig,
		SuiteHasProgrammaticFocus: hasProgrammaticFocus,
		PreRunStats: types.PreRunStats{
			TotalSpecs:       len(specs),
			SpecsThatWillRun: numSpecsThatWillBeRun,
		},
		StartTime: time.Now(),
	}

	reporter.SuiteWillBegin(report)
	if suiteConfig.ParallelTotal > 1 {
		suite.client.PostSuiteWillBegin(report)
	}

	report.SuiteSucceeded = true

	processSpecReport := func(specReport types.SpecReport) {
		reporter.DidRun(specReport)
		if suiteConfig.ParallelTotal > 1 {
			suite.client.PostDidRun(specReport)
		}
		if specReport.State.Is(types.SpecStateFailureStates...) {
			report.SuiteSucceeded = false
		}
		report.SpecReports = append(report.SpecReports, specReport)
	}

	interruptStatus := interruptHandler.Status()
	beforeSuiteNode := suite.suiteNodes.FirstNodeWithType(types.NodeTypeBeforeSuite, types.NodeTypeSynchronizedBeforeSuite)
	if !beforeSuiteNode.IsZero() && !interruptStatus.Interrupted && numSpecsThatWillBeRun > 0 {
		suite.currentSpecReport = types.SpecReport{
			LeafNodeType:       beforeSuiteNode.NodeType,
			LeafNodeLocation:   beforeSuiteNode.CodeLocation,
			GinkgoParallelNode: suiteConfig.ParallelNode,
		}
		reporter.WillRun(suite.currentSpecReport)
		suite.runSuiteNode(beforeSuiteNode, failer, interruptStatus.Channel, interruptHandler, writer, outputInterceptor, suiteConfig)
		processSpecReport(suite.currentSpecReport)
	}

	suiteAborted := false
	if report.SuiteSucceeded {
		nextIndex := MakeNextIndexCounter(suiteConfig)

		for {
			idx, err := nextIndex()
			if err != nil {
				report.SpecialSuiteFailureReasons = append(report.SpecialSuiteFailureReasons, fmt.Sprintf("Failed to iterate over specs:\n%s", err.Error()))
				report.SuiteSucceeded = false
				break
			}
			if idx >= len(specs) {
				break
			}

			spec := specs[idx]

			suite.currentSpecReport = types.SpecReport{
				ContainerHierarchyTexts:     spec.Nodes.WithType(types.NodeTypeContainer).Texts(),
				ContainerHierarchyLocations: spec.Nodes.WithType(types.NodeTypeContainer).CodeLocations(),
				LeafNodeLocation:            spec.FirstNodeWithType(types.NodeTypeIt).CodeLocation,
				LeafNodeType:                types.NodeTypeIt,
				LeafNodeText:                spec.FirstNodeWithType(types.NodeTypeIt).Text,
				GinkgoParallelNode:          suiteConfig.ParallelNode,
			}

			if (suiteConfig.FailFast && !report.SuiteSucceeded) || interruptHandler.Status().Interrupted || suiteAborted {
				spec.Skip = true
			}

			if spec.Skip {
				suite.currentSpecReport.State = types.SpecStateSkipped
				if spec.Nodes.HasNodeMarkedPending() {
					suite.currentSpecReport.State = types.SpecStatePending
				}
			}

			reporter.WillRun(suite.currentSpecReport)

			if !spec.Skip {
				//runSpec updates suite.currentSpecReport directly
				suite.runSpec(spec, failer, interruptHandler, writer, outputInterceptor, suiteConfig)
			}

			//send the spec report to any attached ReportAfterEach blocks - this will update suite.currentSpecReport of failures occur in these blocks
			suite.reportAfterEach(spec, failer, interruptHandler, writer, outputInterceptor, suiteConfig)
			processSpecReport(suite.currentSpecReport)
			if suite.currentSpecReport.State == types.SpecStateAborted {
				suiteAborted = true
			}
			if suiteConfig.ParallelTotal > 1 && (suiteAborted || (suiteConfig.FailFast && !report.SuiteSucceeded)) {
				suite.client.PostAbort()
			}
			suite.currentSpecReport = types.SpecReport{}
		}

		if specs.HasAnySpecsMarkedPending() && suiteConfig.FailOnPending {
			report.SpecialSuiteFailureReasons = append(report.SpecialSuiteFailureReasons, "Detected pending specs and --fail-on-pending is set")
			report.SuiteSucceeded = false
		}
	}

	afterSuiteNode := suite.suiteNodes.FirstNodeWithType(types.NodeTypeAfterSuite, types.NodeTypeSynchronizedAfterSuite)
	if !afterSuiteNode.IsZero() && numSpecsThatWillBeRun > 0 {
		suite.currentSpecReport = types.SpecReport{
			LeafNodeType:       afterSuiteNode.NodeType,
			LeafNodeLocation:   afterSuiteNode.CodeLocation,
			GinkgoParallelNode: suiteConfig.ParallelNode,
		}
		reporter.WillRun(suite.currentSpecReport)
		suite.runSuiteNode(afterSuiteNode, failer, interruptHandler.Status().Channel, interruptHandler, writer, outputInterceptor, suiteConfig)
		processSpecReport(suite.currentSpecReport)
	}

	interruptStatus = interruptHandler.Status()
	if interruptStatus.Interrupted {
		report.SpecialSuiteFailureReasons = append(report.SpecialSuiteFailureReasons, interruptStatus.Cause.String())
		report.SuiteSucceeded = false
	}
	report.EndTime = time.Now()
	report.RunTime = report.EndTime.Sub(report.StartTime)

	if suiteConfig.ParallelNode == 1 {
		for _, node := range suite.suiteNodes.WithType(types.NodeTypeReportAfterSuite) {
			suite.currentSpecReport = types.SpecReport{
				LeafNodeType:       node.NodeType,
				LeafNodeLocation:   node.CodeLocation,
				LeafNodeText:       node.Text,
				GinkgoParallelNode: suiteConfig.ParallelNode,
			}
			reporter.WillRun(suite.currentSpecReport)
			suite.runReportAfterSuiteNode(node, report, failer, interruptHandler, writer, outputInterceptor, suiteConfig)
			processSpecReport(suite.currentSpecReport)
		}
	}

	reporter.SuiteDidEnd(report)
	if suiteConfig.ParallelTotal > 1 {
		suite.client.PostSuiteDidEnd(report)
	}

	return report.SuiteSucceeded
}

// runSpec(spec) mutates currentSpecReport.  this is ugly
// but it allows the user to call CurrentGinkgoSpecDescription and get
// an up-to-date state of the spec **from within a running spec**
func (suite *Suite) runSpec(spec Spec, failer *Failer, interruptHandler InterruptHandlerInterface, writer WriterInterface, outputInterceptor OutputInterceptor, suiteConfig types.SuiteConfig) {
	if suiteConfig.DryRun {
		suite.currentSpecReport.State = types.SpecStatePassed
		return
	}

	writer.Truncate()
	outputInterceptor.StartInterceptingOutput()
	suite.currentSpecReport.StartTime = time.Now()
	maxAttempts := max(1, spec.FlakeAttempts())
	if suiteConfig.FlakeAttempts > 0 {
		maxAttempts = suiteConfig.FlakeAttempts
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		suite.currentSpecReport.NumAttempts = attempt + 1

		if attempt > 0 {
			fmt.Fprintf(writer, "\nGinkgo: Attempt #%d Failed.  Retrying...\n", attempt)
		}

		interruptStatus := interruptHandler.Status()
		deepestNestingLevelAttained := -1
		nodes := spec.Nodes.WithType(types.NodeTypeBeforeEach).SortedByAscendingNestingLevel()
		nodes = nodes.CopyAppend(spec.Nodes.WithType(types.NodeTypeJustBeforeEach).SortedByAscendingNestingLevel()...)
		nodes = nodes.CopyAppend(spec.Nodes.WithType(types.NodeTypeIt)...)

		for _, node := range nodes {
			deepestNestingLevelAttained = max(deepestNestingLevelAttained, node.NestingLevel)
			suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, failer, interruptStatus.Channel, interruptHandler, spec.Nodes.BestTextFor(node), writer, suiteConfig)
			suite.currentSpecReport.RunTime = time.Since(suite.currentSpecReport.StartTime)
			if suite.currentSpecReport.State != types.SpecStatePassed {
				break
			}
		}

		cleanUpNodes := spec.Nodes.WithType(types.NodeTypeJustAfterEach).SortedByDescendingNestingLevel()
		cleanUpNodes = cleanUpNodes.CopyAppend(spec.Nodes.WithType(types.NodeTypeAfterEach).SortedByDescendingNestingLevel()...)
		cleanUpNodes = cleanUpNodes.WithinNestingLevel(deepestNestingLevelAttained)
		for _, node := range cleanUpNodes {
			state, failure := suite.runNode(node, failer, interruptHandler.Status().Channel, interruptHandler, spec.Nodes.BestTextFor(node), writer, suiteConfig)
			suite.currentSpecReport.RunTime = time.Since(suite.currentSpecReport.StartTime)
			if suite.currentSpecReport.State == types.SpecStatePassed || state == types.SpecStateAborted {
				suite.currentSpecReport.State = state
				suite.currentSpecReport.Failure = failure
			}
		}

		suite.currentSpecReport.EndTime = time.Now()
		suite.currentSpecReport.RunTime = suite.currentSpecReport.EndTime.Sub(suite.currentSpecReport.StartTime)
		suite.currentSpecReport.CapturedGinkgoWriterOutput = string(writer.Bytes())
		suite.currentSpecReport.CapturedStdOutErr = outputInterceptor.StopInterceptingAndReturnOutput()

		if suite.currentSpecReport.State == types.SpecStatePassed {
			return
		}
		if interruptHandler.Status().Interrupted {
			return
		}
	}
}

func (suite *Suite) reportAfterEach(spec Spec, failer *Failer, interruptHandler InterruptHandlerInterface, writer WriterInterface, outputInterceptor OutputInterceptor, suiteConfig types.SuiteConfig) {
	nodes := spec.Nodes.WithType(types.NodeTypeReportAfterEach).SortedByDescendingNestingLevel()
	if len(nodes) == 0 {
		return
	}

	for _, node := range nodes {
		writer.Truncate()
		outputInterceptor.StartInterceptingOutput()
		report := suite.currentSpecReport
		node.Body = func() {
			node.ReportAfterEachBody(report)
		}
		interruptHandler.SetInterruptPlaceholderMessage(formatter.Fiw(0, formatter.COLS,
			"{{yellow}}Ginkgo received an interrupt signal but is currently running a ReportAfterEach node.  To avoid an invalid report the ReportAfterEach node will not be interrupted however subsequent tests will be skipped.{{/}}\n\n{{bold}}The running ReportAfterEach node is at:\n%s.{{/}}",
			node.CodeLocation,
		))
		state, failure := suite.runNode(node, failer, nil, nil, spec.Nodes.BestTextFor(node), writer, suiteConfig)
		interruptHandler.ClearInterruptPlaceholderMessage()
		if suite.currentSpecReport.State == types.SpecStatePassed || state == types.SpecStateAborted {
			suite.currentSpecReport.State = state
			suite.currentSpecReport.Failure = failure
		}
		suite.currentSpecReport.CapturedGinkgoWriterOutput += string(writer.Bytes())
		suite.currentSpecReport.CapturedStdOutErr += outputInterceptor.StopInterceptingAndReturnOutput()
	}
}

func (suite *Suite) runSuiteNode(node Node, failer *Failer, interruptChannel chan interface{}, interruptHandler InterruptHandlerInterface, writer WriterInterface, outputInterceptor OutputInterceptor, suiteConfig types.SuiteConfig) {
	if suiteConfig.DryRun {
		suite.currentSpecReport.State = types.SpecStatePassed
		return
	}

	writer.Truncate()
	outputInterceptor.StartInterceptingOutput()
	suite.currentSpecReport.StartTime = time.Now()

	var err error
	switch node.NodeType {
	case types.NodeTypeBeforeSuite, types.NodeTypeAfterSuite:
		suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, failer, interruptChannel, interruptHandler, "", writer, suiteConfig)
	case types.NodeTypeSynchronizedBeforeSuite:
		var data []byte
		var runAllNodes bool
		if suiteConfig.ParallelNode == 1 {
			node.Body = func() { data = node.SynchronizedBeforeSuiteNode1Body() }
			suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, failer, interruptChannel, interruptHandler, "", writer, suiteConfig)
			if suiteConfig.ParallelTotal > 1 && suite.currentSpecReport.State.Is(types.SpecStatePassed) {
				err = suite.client.PostSynchronizedBeforeSuiteSucceeded(data)
			} else if suiteConfig.ParallelTotal > 1 {
				err = suite.client.PostSynchronizedBeforeSuiteFailed()
			}
			runAllNodes = suite.currentSpecReport.State.Is(types.SpecStatePassed) && err == nil
		} else {
			data, err = suite.client.BlockUntilSynchronizedBeforeSuiteData()
			runAllNodes = err == nil
		}
		if runAllNodes {
			node.Body = func() { node.SynchronizedBeforeSuiteAllNodesBody(data) }
			suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, failer, interruptChannel, interruptHandler, "", writer, suiteConfig)
		}
	case types.NodeTypeSynchronizedAfterSuite:
		node.Body = node.SynchronizedAfterSuiteAllNodesBody
		suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, failer, interruptChannel, interruptHandler, "", writer, suiteConfig)
		if suiteConfig.ParallelNode == 1 {
			if suiteConfig.ParallelTotal > 1 {
				err = suite.client.BlockUntilNonprimaryNodesHaveFinished()
			}
			if err == nil {
				node.Body = node.SynchronizedAfterSuiteNode1Body
				state, failure := suite.runNode(node, failer, interruptChannel, interruptHandler, "", writer, suiteConfig)
				if suite.currentSpecReport.State.Is(types.SpecStatePassed) {
					suite.currentSpecReport.State, suite.currentSpecReport.Failure = state, failure
				}
			}
		}
	}

	if err != nil && suite.currentSpecReport.State.Is(types.SpecStateInvalid, types.SpecStatePassed) {
		suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.failureForLeafNodeWithError(node, err)
	}

	suite.currentSpecReport.EndTime = time.Now()
	suite.currentSpecReport.RunTime = suite.currentSpecReport.EndTime.Sub(suite.currentSpecReport.StartTime)
	suite.currentSpecReport.CapturedGinkgoWriterOutput = string(writer.Bytes())
	suite.currentSpecReport.CapturedStdOutErr = outputInterceptor.StopInterceptingAndReturnOutput()

	return
}

func (suite *Suite) runReportAfterSuiteNode(node Node, report types.Report, failer *Failer, interruptHandler InterruptHandlerInterface, writer WriterInterface, outputInterceptor OutputInterceptor, suiteConfig types.SuiteConfig) {
	if suiteConfig.DryRun {
		suite.currentSpecReport.State = types.SpecStatePassed
		return
	}

	writer.Truncate()
	outputInterceptor.StartInterceptingOutput()
	suite.currentSpecReport.StartTime = time.Now()

	if suiteConfig.ParallelTotal > 1 {
		aggregatedReport, err := suite.client.BlockUntilAggregatedNonprimaryNodesReport()
		if err != nil {
			suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.failureForLeafNodeWithError(node, err)
			return
		}
		report = report.Add(aggregatedReport)
	}

	node.Body = func() { node.ReportAfterSuiteBody(report) }
	interruptHandler.SetInterruptPlaceholderMessage(formatter.Fiw(0, formatter.COLS,
		"{{yellow}}Ginkgo received an interrupt signal but is currently running a ReportAfterSuite node.  To avoid an invalid report the ReportAfterSuite node will not be interrupted.{{/}}\n\n{{bold}}The running ReportAfterSuite node is at:\n%s.{{/}}",
		node.CodeLocation,
	))
	suite.currentSpecReport.State, suite.currentSpecReport.Failure = suite.runNode(node, failer, nil, nil, "", writer, suiteConfig)
	interruptHandler.ClearInterruptPlaceholderMessage()

	suite.currentSpecReport.EndTime = time.Now()
	suite.currentSpecReport.RunTime = suite.currentSpecReport.EndTime.Sub(suite.currentSpecReport.StartTime)
	suite.currentSpecReport.CapturedGinkgoWriterOutput = string(writer.Bytes())
	suite.currentSpecReport.CapturedStdOutErr = outputInterceptor.StopInterceptingAndReturnOutput()

	return
}

func (suite *Suite) runNode(node Node, failer *Failer, interruptChannel chan interface{}, interruptHandler InterruptHandlerInterface, text string, writer WriterInterface, suiteConfig types.SuiteConfig) (types.SpecState, types.Failure) {
	if suiteConfig.EmitSpecProgress {
		if text == "" {
			text = "TOP-LEVEL"
		}
		s := fmt.Sprintf("[%s] %s\n  %s\n", node.NodeType.String(), text, node.CodeLocation.String())
		writer.Write([]byte(s))
	}

	var failure types.Failure
	failure.FailureNodeType, failure.FailureNodeLocation = node.NodeType, node.CodeLocation
	if node.NodeType.Is(types.NodeTypeIt) || node.NodeType.Is(types.NodeTypesForSuiteLevelNodes...) {
		failure.FailureNodeContext = types.FailureNodeIsLeafNode
	} else if node.NestingLevel <= 0 {
		failure.FailureNodeContext = types.FailureNodeAtTopLevel
	} else {
		failure.FailureNodeContext, failure.FailureNodeContainerIndex = types.FailureNodeInContainer, node.NestingLevel-1
	}

	outcomeC := make(chan types.SpecState)
	failureC := make(chan types.Failure)

	go func() {
		finished := false
		defer func() {
			if e := recover(); e != nil || !finished {
				failer.Panic(types.NewCodeLocationWithStackTrace(2), e)
			}

			outcome, failureFromRun := failer.Drain()
			outcomeC <- outcome
			failureC <- failureFromRun
		}()

		node.Body()
		finished = true
	}()

	select {
	case outcome := <-outcomeC:
		failureFromRun := <-failureC
		if outcome == types.SpecStatePassed {
			return outcome, types.Failure{}
		}
		failure.Message, failure.Location, failure.ForwardedPanic = failureFromRun.Message, failureFromRun.Location, failureFromRun.ForwardedPanic
		return outcome, failure
	case <-interruptChannel:
		failure.Message, failure.Location = interruptHandler.InterruptMessageWithStackTraces(), node.CodeLocation
		return types.SpecStateInterrupted, failure
	}
}

func (suite *Suite) failureForLeafNodeWithError(node Node, err error) (types.SpecState, types.Failure) {
	return types.SpecStateFailed, types.Failure{
		Message:             err.Error(),
		Location:            node.CodeLocation,
		FailureNodeContext:  types.FailureNodeIsLeafNode,
		FailureNodeType:     node.NodeType,
		FailureNodeLocation: node.CodeLocation,
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
