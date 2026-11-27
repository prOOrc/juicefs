//go:build windows
// +build windows

/*
 * JuiceFS, Copyright 2026 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Обёртки CfExecute — операции провайдера (план задач: файл operations.go
// наполняется этапами 3+ по потребности; на этапе 2 — ответ на
// FETCH_PLACEHOLDERS, design.md D5/D6).
package cfapi

import (
	"unsafe"
)

// AnswerFetchPlaceholders отвечает на задание FETCH_PLACEHOLDERS операцией
// TRANSFER_PLACEHOLDERS: записи создаются фильтром относительно каталога,
// с которым связан TransferKey (RelativeFileName каждой записи — имя внутри
// этого каталога, без разделителей пути).
//
// status — CompletionStatus ответа (StatusSuccess, при отмене — код отказа);
// placeholders — записи плейсхолдеров (nil допустим, например при отмене);
// total — PlaceholderTotalCount: полное число записей каталога (не EOF, см.
// провайдера); entriesProcessed — EntriesProcessed: кумулятивный счётчик
// обработанных провайдером записей. Живая проба 27.11: фильтр завершает
// population по processed >= total — single-shot ответ должен нести
// processed = полное число записей каталога (== count, когда каталог
// передаётся целиком), иначе create пендится вечно.
//
// Ответ обязателен на каждое задание ровно один раз — иначе перечисление
// каталога в Explorer остаётся подвешенным до таймаута фильтра.
func AnswerFetchPlaceholders(conn ConnectionKey, transferKey TransferKey, requestKey RequestKey, status NTStatus, placeholders []CFPlaceholderCreateInfo, total int64, entriesProcessed uint32) error {
	opInfo := CFOperationInfo{
		StructSize:    uint32(unsafe.Sizeof(CFOperationInfo{})),
		Type:          OperationTypeTransferPlaceholders,
		ConnectionKey: conn,
		TransferKey:   transferKey,
		RequestKey:    requestKey,
	}
	opParams := CFOperationParameters{
		// ParamSize — размер ВСЕЙ структуры параметров (union целиком).
		// cfapi.h ParamSize не документирует; в MS-сэмпле CloudMirror
		// встречается и размер активной ветки (sizeof(opParams.TransferData)).
		// Живые пробы U2 (27.11): (1) ParamSize=32 (размер ветки
		// TransferPlaceholders) при count=0/NULL-массиве — E_INVALIDARG;
		// (2) с union-размером count=0+SUCCESS принят фильтром — легальный
		// ответ «населено, можно создавать» (на provider'е: пустой каталог
		// или каталог, всё содержимое которого уже на диске). Если union
		// целиком когда-нибудь отвергается — вернуть размер ветки.
		ParamSize: uint32(unsafe.Sizeof(CFOperationParameters{})),
	}
	p := opParams.OperationTransferPlaceholders()
	p.Flags = OperationTransferPlaceholdersFlagNone
	p.CompletionStatus = status
	p.PlaceholderTotalCount = total
	if len(placeholders) > 0 {
		p.PlaceholderArray = &placeholders[0]
	}
	p.PlaceholderCount = uint32(len(placeholders))
	p.EntriesProcessed = entriesProcessed
	return CfExecute(&opInfo, &opParams)
}
