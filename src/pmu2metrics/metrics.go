package main

import (
	"fmt"
	"log"
	"strings"

	"github.com/Knetic/govaluate"
	mapset "github.com/deckarep/golang-set/v2"
)

type Metric struct {
	Name  string
	Value float64
}

func loadMetricBestGroups(metric *MetricDefinition, frame EventFrame) (err error) {
	allVariableNames := mapset.NewSetFromMapKeys(metric.Variables)
	remainingVariableNames := allVariableNames.Clone()
	for {
		if remainingVariableNames.Cardinality() == 0 { // found matches for all
			break
		}
		// find group with the greatest number of event names that match the remaining variable names
		bestGroupIdx := -1
		bestMatches := 0
		var matchedNames mapset.Set[string] // := mapset.NewSet([]string{}...)
		for groupIdx, group := range frame.EventGroups {
			groupEventNames := mapset.NewSetFromMapKeys(group.EventValues)
			intersection := remainingVariableNames.Intersect(groupEventNames)
			if intersection.Cardinality() > bestMatches {
				bestGroupIdx = groupIdx
				bestMatches = intersection.Cardinality()
				matchedNames = intersection.Clone()
				if bestMatches == remainingVariableNames.Cardinality() {
					break
				}
			}
		}
		if bestGroupIdx == -1 { // no matches
			for _, variableName := range remainingVariableNames.ToSlice() {
				metric.Variables[variableName] = -2 // we tried and failed
			}
			err = fmt.Errorf("metric variables (%s) not found for metric: %s", strings.Join(remainingVariableNames.ToSlice(), ", "), metric.Name)
			break
		}
		// for each of the matched names, set the value and the group from which to retrieve the value next time
		for _, name := range matchedNames.ToSlice() {
			metric.Variables[name] = bestGroupIdx
		}
		remainingVariableNames = remainingVariableNames.Difference(matchedNames)
	}
	return
}

// get the variable names & values that will be used to evaluate the metric's expression
func getExpressionVariables(metric MetricDefinition, frame EventFrame, previousTimestamp float64, metadata Metadata) (variables map[string]interface{}, err error) {
	variables = make(map[string]interface{})
	// if first frame, we'll need to determine the best groups from which to get event values for the variables
	loadGroups := false
	for variableName := range metric.Variables {
		if metric.Variables[variableName] == -1 { // group not yet set
			loadGroups = true
		}
		if metric.Variables[variableName] == -2 { // tried previously and failed, don't try again
			errstr := fmt.Sprintf("metric variable group assignment previously failed, skipping: %s", variableName)
			err = fmt.Errorf(errstr)
			if gVerbose {
				log.Print(errstr)
			}
			return
		}
	}
	if loadGroups {
		if err = loadMetricBestGroups(&metric, frame); err != nil {
			err = fmt.Errorf("at least one of the variables couldn't be assigned to a group: %v", err)
			return
		}
	}
	// set the variable values to be used in the expression evaluation
	for variableName := range metric.Variables {
		if metric.Variables[variableName] == -2 {
			err = fmt.Errorf("variable value set to -2 (shouldn't happen): %s", variableName)
		}
		// set the variable value to the event value divided by the perf collection time to normalize the value to 1 second
		variables[variableName] = frame.EventGroups[metric.Variables[variableName]].EventValues[variableName] / (frame.Timestamp - previousTimestamp)
		// adjust cstate_core/c6-residency value if hyperthreading is enabled
		// why here? so we don't have to change the perfmon metric formula
		if metadata.ThreadsPerCore > 1 && variableName == "cstate_core/c6-residency/" {
			variables[variableName] = variables[variableName].(float64) * float64(metadata.ThreadsPerCore)
		}
	}
	return
}

// define functions that can be called in metric expressions
func getEvaluatorFunctions() (functions map[string]govaluate.ExpressionFunction) {
	functions = make(map[string]govaluate.ExpressionFunction)
	functions["max"] = func(args ...interface{}) (interface{}, error) {
		var leftVal float64
		var rightVal float64
		switch t := args[0].(type) {
		case int:
			leftVal = float64(t)
		case float64:
			leftVal = t
		}
		switch t := args[1].(type) {
		case int:
			rightVal = float64(t)
		case float64:
			rightVal = t
		}
		return max(leftVal, rightVal), nil
	}
	functions["min"] = func(args ...interface{}) (interface{}, error) {
		var leftVal float64
		var rightVal float64
		switch t := args[0].(type) {
		case int:
			leftVal = float64(t)
		case float64:
			leftVal = t
		}
		switch t := args[1].(type) {
		case int:
			rightVal = float64(t)
		case float64:
			rightVal = t
		}
		return min(leftVal, rightVal), nil
	}
	return
}

// function to call evaluator so that we can catch panics that come from the evaluator
func evaluateExpression(metric MetricDefinition, variables map[string]interface{}, functions map[string]govaluate.ExpressionFunction) (result interface{}, err error) {
	defer func() {
		if errx := recover(); errx != nil {
			err = errx.(error)
		}
	}()
	if metric.EvaluatorExpression == nil {
		var evExpression *govaluate.EvaluableExpression
		if evExpression, err = govaluate.NewEvaluableExpressionWithFunctions(metric.Expression, functions); err != nil {
			log.Printf("%v: %s", err, metric.Expression)
			return
		}
		metric.EvaluatorExpression = evExpression // save this so we don't have to create it again for the same metric
	}
	if result, err = metric.EvaluatorExpression.Evaluate(variables); err != nil {
		log.Printf("%v: %s", err, metric.Expression)
	}
	return
}

func processEvents(perfEvents []string, metricDefinitions []MetricDefinition, functions map[string]govaluate.ExpressionFunction, previousTimestamp float64, metadata Metadata) (metrics []Metric, timeStamp float64, err error) {
	var eventFrame EventFrame
	if eventFrame, err = getEventFrame(perfEvents); err != nil { // arrange the events into groups
		err = fmt.Errorf("failed to put perf events into groups: %v", err)
	}
	timeStamp = eventFrame.Timestamp
	// produce metrics from event groups
	for _, metricDef := range metricDefinitions {
		var variables map[string]interface{}
		if variables, err = getExpressionVariables(metricDef, eventFrame, previousTimestamp, metadata); err != nil {
			// Note: err is logged by getExpressionVariables
			err = nil
			continue
		}
		var result interface{}
		if result, err = evaluateExpression(metricDef, variables, functions); err != nil {
			// Note: err is logged by evaluateExpression
			err = nil
			continue
		}
		metrics = append(metrics, Metric{Name: metricDef.Name, Value: result.(float64)})
		if gVerbose {
			var prettyVars []string
			for variableName := range variables {
				prettyVars = append(prettyVars, fmt.Sprintf("%s=%f", variableName, variables[variableName]))
			}
			log.Printf("%s : %s : %s", metricDef.Name, metricDef.Expression, strings.Join(prettyVars, ", "))
		}
	}
	return
}
