// Copyright 2017 Xiaomi, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import (
	"encoding/json"
	"fmt"
	"github.com/open-falcon/falcon-plus/common/model"
	"github.com/open-falcon/falcon-plus/modules/judge/g"
	"log"
)

func Judge(L *SafeLinkedList, firstItem *model.JudgeItem, now int64) {
	CheckStrategy(L, firstItem, now)
	CheckExpression(L, firstItem, now)
}

func CheckStrategy(L *SafeLinkedList, firstItem *model.JudgeItem, now int64) {
	key := fmt.Sprintf("%s/%s", firstItem.Endpoint, firstItem.Metric)
	strategyMap := g.StrategyMap.Get()
	strategies, exists := strategyMap[key]
	if !exists {
		return
	}

	metricLevelMap := make(map[string]bool)
	//kingsgroup修改: 用于记录相同metric,不同报警级别的检测结果，如果critical级别检测到报警，则不再对warning的检测

	for _, s := range strategies {
		// 因为key仅仅是endpoint和metric，所以得到的strategies并不一定是与当前judgeItem相关的
		// 比如lg-dinp-docker01.bj配置了两个proc.num的策略，一个name=docker，一个name=agent
		// 所以此处要排除掉一部分
		related := true
		for tagKey, tagVal := range s.Tags {
			if myVal, exists := firstItem.Tags[tagKey]; !exists || myVal != tagVal {
				related = false
				break
			}
		}

		if !related {
			continue
		}

		//kingsgroup修改: 开始检测并记录报警结果
		metric := s.Metric
		level := s.Priority
		key := fmt.Sprintf("%s_%d", metric, level)
		if level > 0 {
			// 只有priority大于0时才检测，如果是0(表示最高级别)总是去检测
			need_check_flag := true //标记检测结果, 默认需要检测报警
			for i := 0; i < level; i++ {
				// 只检查高于当次priority的检测结果
				check_key := fmt.Sprintf("%s_%d", metric, i)
				if ret, exists := metricLevelMap[check_key]; exists && ret {
					// 如果已经在检测结果中，并且结果为true,代表不用再检测低级别的表达式
					need_check_flag = false
					break
				}
			}
			if !need_check_flag {
				continue
			}
		}

		metricLevelMap[key] = judgeItemWithStrategy(L, s, firstItem, now)
	}
}

func judgeItemWithStrategy(L *SafeLinkedList, strategy model.Strategy, firstItem *model.JudgeItem, now int64) bool {
	fn, err := ParseFuncFromString(strategy.Func, strategy.Operator, strategy.RightValue)
	if err != nil {
		log.Printf("[ERROR] parse func %s fail: %v. strategy id: %d", strategy.Func, err, strategy.Id)
		return false
	}

	historyData, leftValue, isTriggered, isEnough := fn.Compute(L)
	if !isEnough {
		return false
	}

	event := &model.Event{
		Id:         fmt.Sprintf("s_%d_%s", strategy.Id, firstItem.PrimaryKey()),
		Strategy:   &strategy,
		Endpoint:   firstItem.Endpoint,
		LeftValue:  leftValue,
		EventTime:  firstItem.Timestamp,
		PushedTags: firstItem.Tags,
	}

	return sendEventIfNeed(historyData, isTriggered, now, event, strategy.MaxStep)
}

func sendEvent(event *model.Event) {
	// update last event
	g.LastEvents.Set(event.Id, event)

	bs, err := json.Marshal(event)
	if err != nil {
		log.Printf("json marshal event %v fail: %v", event, err)
		return
	}

	// send to redis
	// redisKey := fmt.Sprintf(g.Config().Alarm.QueuePattern, event.Priority())

	// redisKey := fmt.Sprintf(g.Config().Alarm.QueuePattern, event.Category())
	// kingsgroup修改，根据事件的配置的策略category，写入到不同的队列, event:category_name
	redisKey := g.Config().Alarm.Queue // 增加queue配置项
	rc := g.RedisConnPool.Get()
	defer rc.Close()
	rc.Do("LPUSH", redisKey, string(bs))
}

func CheckExpression(L *SafeLinkedList, firstItem *model.JudgeItem, now int64) {
	keys := buildKeysFromMetricAndTags(firstItem)
	if len(keys) == 0 {
		return
	}

	// expression可能会被多次重复处理，用此数据结构保证只被处理一次
	handledExpression := make(map[int]struct{})

	expressionMap := g.ExpressionMap.Get()
	for _, key := range keys {
		expressions, exists := expressionMap[key]
		if !exists {
			continue
		}

		metricLevelMap := make(map[string]bool)
		//kingsgroup修改: 用于记录相同metric,不同报警级别的检测结果，如果critical级别检测到报警，则不再对warning的检测

		related := filterRelatedExpressions(expressions, firstItem)
		for _, exp := range related {
			if _, ok := handledExpression[exp.Id]; ok {
				continue
			}
			handledExpression[exp.Id] = struct{}{}

			//kingsgroup修改: 开始检测并记录报警结果
			metric := exp.Metric
			level := exp.Priority
			key := fmt.Sprintf("%s_%d", metric, level)
			if level > 0 {
				// 只有priority大于0时才检测，如果是0(表示最高级别)总是去检测
				need_check_flag := true //标记检测结果, 默认需要检测报警
				for i := 0; i < level; i++ {
					// 只检查高于当次priority的检测结果
					check_key := fmt.Sprintf("%s_%d", metric, i)
					if ret, exists := metricLevelMap[check_key]; exists && ret {
						// 如果已经在检测结果中，并且结果为true,代表不用再检测低级别的表达式
						need_check_flag = false
						break
					}
				}
				if !need_check_flag {
					continue
				}
			}

			metricLevelMap[key] = judgeItemWithExpression(L, exp, firstItem, now)
		}
	}
}

func buildKeysFromMetricAndTags(item *model.JudgeItem) (keys []string) {
	for k, v := range item.Tags {
		keys = append(keys, fmt.Sprintf("%s/%s=%s", item.Metric, k, v))
	}
	keys = append(keys, fmt.Sprintf("%s/endpoint=%s", item.Metric, item.Endpoint))
	return
}

func filterRelatedExpressions(expressions []*model.Expression, firstItem *model.JudgeItem) []*model.Expression {
	size := len(expressions)
	if size == 0 {
		return []*model.Expression{}
	}

	exps := make([]*model.Expression, 0, size)

	for _, exp := range expressions {

		related := true

		itemTagsCopy := firstItem.Tags
		// 注意：exp.Tags 中可能会有一个endpoint=xxx的tag
		if _, ok := exp.Tags["endpoint"]; ok {
			itemTagsCopy = copyItemTags(firstItem)
		}

		for tagKey, tagVal := range exp.Tags {
			if myVal, exists := itemTagsCopy[tagKey]; !exists || myVal != tagVal {
				related = false
				break
			}
		}

		if !related {
			continue
		}

		exps = append(exps, exp)
	}

	return exps
}

func copyItemTags(item *model.JudgeItem) map[string]string {
	ret := make(map[string]string)
	ret["endpoint"] = item.Endpoint
	if item.Tags != nil && len(item.Tags) > 0 {
		for k, v := range item.Tags {
			ret[k] = v
		}
	}
	return ret
}

func judgeItemWithExpression(L *SafeLinkedList, expression *model.Expression, firstItem *model.JudgeItem, now int64) bool {
	fn, err := ParseFuncFromString(expression.Func, expression.Operator, expression.RightValue)
	if err != nil {
		log.Printf("[ERROR] parse func %s fail: %v. expression id: %d", expression.Func, err, expression.Id)
		return false
	}

	historyData, leftValue, isTriggered, isEnough := fn.Compute(L)
	if !isEnough {
		return false
	}

	event := &model.Event{
		Id:         fmt.Sprintf("e_%d_%s", expression.Id, firstItem.PrimaryKey()),
		Expression: expression,
		Endpoint:   firstItem.Endpoint,
		LeftValue:  leftValue,
		EventTime:  firstItem.Timestamp,
		PushedTags: firstItem.Tags,
	}

	return sendEventIfNeed(historyData, isTriggered, now, event, expression.MaxStep)

}

func sendEventIfNeed(historyData []*model.HistoryData, isTriggered bool, now int64, event *model.Event, maxStep int) bool {
	lastEvent, exists := g.LastEvents.Get(event.Id)
	if isTriggered {
		event.Status = "PROBLEM"
		if !exists || lastEvent.Status[0] == 'O' {
			// 本次触发了阈值，之前又没报过警，得产生一个报警Event
			event.CurrentStep = 1

			// 但是有些用户把最大报警次数配置成了0，相当于屏蔽了，要检查一下
			// kingsgroup修改，取消最大报警次数的限制
			// if maxStep == 0 {
			// 	return
			// }

			sendEvent(event)
			return true
		}

		// 逻辑走到这里，说明之前Event是PROBLEM状态
		// kingsgroup修改，取消最大报警次数的限制
		// if lastEvent.CurrentStep >= maxStep {
		// 	// 报警次数已经足够多，到达了最多报警次数了，不再报警
		// 	return
		// }

		if historyData[len(historyData)-1].Timestamp <= lastEvent.EventTime {
			// 产生过报警的点，就不能再使用来判断了，否则容易出现一分钟报一次的情况
			// 只需要拿最后一个historyData来做判断即可，因为它的时间最老
			return false
		}

		// kingsgroup修改, 报警间隔由duty控制，取消这里的限制
		// if now-lastEvent.EventTime < g.Config().Alarm.MinInterval {
		// 	// 报警不能太频繁，两次报警之间至少要间隔MinInterval秒，否则就不能报警
		// 	return
		// }

		event.CurrentStep = lastEvent.CurrentStep + 1
		sendEvent(event)
		return true
	} else {
		// 如果LastEvent是Problem，报OK，否则啥都不做
		if exists && lastEvent.Status[0] == 'P' {
			event.Status = "OK"
			event.CurrentStep = 1
			sendEvent(event)
			return true
		}
		return false
	}
}
