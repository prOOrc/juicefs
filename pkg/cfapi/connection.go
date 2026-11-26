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

package cfapi

import "fmt"

// SyncRootAPI — поверхность cldapi.dll, через которую провайдер
// (pkg/cfprovider) и команды clouddrive общаются с фильтром. Реальная
// реализация — OS (пакетные обёртки этого пакета); подменная — в тестах:
// движок проверяется без Cloud Files API (design.md, инвариант «Движок
// проверяется без CfAPI»).
//
// Подключение обязано идти через Connect: обычная обёртка CfConnectSyncRoot
// не удерживает таблицу callback'ов после возврата (см. Connection).
type SyncRootAPI interface {
	// Available проверяет, что cldapi.dll загрузилась.
	Available() error
	// RegisterSyncRoot регистрирует sync root (CF_REGISTER_FLAGS).
	RegisterSyncRoot(syncRootPath string, registration *CFSyncRegistration, policies *CFSyncPolicies, registerFlags uint32) error
	// UnregisterSyncRoot снимает регистрацию sync root.
	UnregisterSyncRoot(syncRootPath string) error
	// GetSyncRootInfoByPath читает информацию sync root по пути
	// (CF_SYNC_ROOT_INFO_CLASS); ошибка — путь вне зарегистрированного sync root.
	GetSyncRootInfoByPath(filePath string, infoClass uint32, infoBuffer []byte, returnedLength *uint32) error
	// Connect подключает провайдер; возвращённый *Connection удерживает
	// таблицу callback'ов и callback-контекст всё время соединения.
	Connect(syncRootPath string, callbackTable []CFCallbackRegistration, callbackContext uintptr, connectFlags uint32) (*Connection, error)
	// DisconnectSyncRoot разрывает соединение (CfDisconnectSyncRoot).
	DisconnectSyncRoot(connectionKey ConnectionKey) error
}

// OS — реализация SyncRootAPI на пакетных обёртках cldapi.dll; nil-значение
// api провайдера означает именно её.
var OS SyncRootAPI = osAPI{}

type osAPI struct{}

func (osAPI) Available() error {
	return Available()
}

func (osAPI) RegisterSyncRoot(syncRootPath string, registration *CFSyncRegistration, policies *CFSyncPolicies, registerFlags uint32) error {
	return CfRegisterSyncRoot(syncRootPath, registration, policies, registerFlags)
}

func (osAPI) UnregisterSyncRoot(syncRootPath string) error {
	return CfUnregisterSyncRoot(syncRootPath)
}

func (osAPI) GetSyncRootInfoByPath(filePath string, infoClass uint32, infoBuffer []byte, returnedLength *uint32) error {
	return CfGetSyncRootInfoByPath(filePath, infoClass, infoBuffer, returnedLength)
}

func (osAPI) Connect(syncRootPath string, callbackTable []CFCallbackRegistration, callbackContext uintptr, connectFlags uint32) (*Connection, error) {
	return Connect(syncRootPath, callbackTable, callbackContext, connectFlags)
}

func (osAPI) DisconnectSyncRoot(connectionKey ConnectionKey) error {
	return CfDisconnectSyncRoot(connectionKey)
}

// Connection — соединение провайдера с sync root.
//
// Контракт времени жизни (design.md D7): фильтр cldflt.sys читает таблицу
// callback'ов и callback-контекст ПОСЛЕ возврата CfConnectSyncRoot — всё
// время соединения, вплоть до CfDisconnectSyncRoot. Поэтому Connect копирует
// таблицу в поле Connection и хранит callbackContext рядом: Go не освободит
// эти данные, пока объект соединения жив. Передавать во временном срезе
// (или во временной структуре) таблицу/контекст запрещено — обёртка
// CfConnectSyncRoot без удержания предназначена только для вызова из Connect.
type Connection struct {
	SyncRootPath  string
	Key           ConnectionKey // заполняется фильтром при Connect
	callbackTable []CFCallbackRegistration
	// callbackContext — значение, переданное фильтру; фильтр возвращает его
	// в CF_CALLBACK_INFO.CallbackContext каждого callback'а.
	callbackContext uintptr
}

// Connect подключает провайдер к sync root и возвращает соединение. Срез
// callbackTable копируется внутрь Connection (удержание всё соединение —
// комментарий Connection); callbackContext фиксируется там же. До вызова
// установите приёмник заданий (SetDispatcher): callback'и могут прийти
// сразу после подключения.
func Connect(syncRootPath string, callbackTable []CFCallbackRegistration, callbackContext uintptr, connectFlags uint32) (*Connection, error) {
	conn := &Connection{
		SyncRootPath:    syncRootPath,
		callbackTable:   append([]CFCallbackRegistration(nil), callbackTable...),
		callbackContext: callbackContext,
	}
	if err := CfConnectSyncRoot(syncRootPath, conn.callbackTable, conn.callbackContext, connectFlags, &conn.Key); err != nil {
		return nil, err
	}
	return conn, nil
}

// Close разрывает соединение (CfDisconnectSyncRoot) — реальный вызов
// cldapi.dll, поэтому тесты движка его не используют: провайдер закрывает
// соединение через SyncRootAPI (подменная реализация считает вызовы).
func (c *Connection) Close() error {
	if c == nil {
		return nil
	}
	if err := CfDisconnectSyncRoot(c.Key); err != nil {
		return fmt.Errorf("close connection for %s: %w", c.SyncRootPath, err)
	}
	return nil
}
