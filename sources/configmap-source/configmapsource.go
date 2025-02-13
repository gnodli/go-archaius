/*
 * Copyright 2019 Huawei Technologies Co., Ltd
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package configmapource

import (
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/go-chassis/go-archaius/core"
	"github.com/go-chassis/go-archaius/sources/utils"
	"github.com/go-mesh/openlogging"
	"strings"
	"time"
)

const (
	//ConfigMapConfigSourceConst is a variable of type string
	ConfigMapConfigSourceConst = "ConfigMapSource"
	configMapSourcePriority    = 4
	//DefaultConfigMapPriority as default priority
	DefaultConfigMapPriority = 0
)

//ConfigMapFileSourceTypes is a string
type ConfigMapFileSourceTypes string

const (
	//RegularFile as regular file
	RegularFile ConfigMapFileSourceTypes = "RegularFile"
	//Directory is directory
	Directory ConfigMapFileSourceTypes = "Directory"
	//InvalidFileType type InvalidType
	InvalidFileType ConfigMapFileSourceTypes = "InvalidType"
)

//ConfigInfo is s struct
type ConfigInfo struct {
	FilePath string
	Value    interface{}
}

type configMapSource struct {
	Configurations map[string]*ConfigInfo
	files          []file
	fileHandlers   map[string]utils.FileHandler
	watchPool      *watch
	fileLock       sync.Mutex
	priority       int
	sync.RWMutex
}

type file struct {
	filePath string
	priority uint32
}

type watch struct {
	watcher         *fsnotify.Watcher
	callback        core.DynamicConfigCallback
	configMapSource *configMapSource
	sync.RWMutex
}

var _ core.ConfigSource = &configMapSource{}
var _ ConfigMapSource = &configMapSource{}

var configMapConfigSource *configMapSource

//ConfigMapSource is interface
type ConfigMapSource interface {
	core.ConfigSource
	AddFile(filePath string, priority uint32, handler utils.FileHandler) error
}

//NewConfigMapSource creates a source which can handler recurse directory
func NewConfigMapSource() ConfigMapSource {
	if configMapConfigSource == nil {
		configMapConfigSource = new(configMapSource)
		configMapConfigSource.priority = configMapSourcePriority
		configMapConfigSource.files = make([]file, 0)
		configMapConfigSource.fileHandlers = make(map[string]utils.FileHandler)
	}

	return configMapConfigSource
}

func (cmSource *configMapSource) AddFile(p string, priority uint32, handle utils.FileHandler) error {

	path, err := cmSource.getFilePath(p)
	if err != nil {
		return err
	}

	if cmSource.isFileSrcExist(path) {
		return nil
	}
	cmSource.fileHandlers[path] = handle

	err = filepath.Walk(p,
		func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			path, err = filepath.Abs(p)
			if err != nil {
				return err
			}

			fs, err := os.Open(path)
			if os.IsNotExist(err) {
				return fmt.Errorf("[%s] file not exist", path)
			}
			defer fs.Close()

			getFileType := getFileType(fs)
			switch getFileType {
			case Directory:
				if cmSource.watchPool != nil {
					cmSource.watchPool.AddWatchFile(path)
				}
			case RegularFile:
				err := cmSource.handleFile(fs, priority, handle)
				if cmSource.watchPool != nil {
					cmSource.watchPool.AddWatchFile(path)
				}
				if err != nil {
					openlogging.GetLogger().Errorf("Failed to handle file [%s] [%s]", path, err)
					return err
				}
			case InvalidFileType:
				openlogging.GetLogger().Errorf("File type of [%s] not supported: %s", path, err)
				return fmt.Errorf("file type of [%s] not supported", path)
			}

			return nil
		})

	return nil
}

func (cmSource *configMapSource) getFilePath(filePath string) (string, error) {
	path, err := filepath.Abs(filePath)
	if err != nil {
		return path, err
	}

	fs, err := os.Open(path)
	if os.IsNotExist(err) {
		return path, fmt.Errorf("[%s] file not exist", path)
	}
	defer fs.Close()
	return path, nil
}

func (cmSource *configMapSource) isFileSrcExist(filePath string) bool {
	var exist bool
	for _, file := range cmSource.files {
		if filePath == file.filePath {
			return true
		}
	}

	return exist
}

func getFileType(fs *os.File) ConfigMapFileSourceTypes {
	fileInfo, err := fs.Stat()
	if err != nil {
		return InvalidFileType
	}

	fileMode := fileInfo.Mode()

	if fileMode.IsDir() {
		return Directory
	} else if fileMode.IsRegular() {
		return RegularFile
	}

	return InvalidFileType
}

func (cmSource *configMapSource) handleFile(file *os.File, priority uint32, handle utils.FileHandler) error {
	Content, err := ioutil.ReadFile(file.Name())
	if err != nil {
		return err
	}
	var config map[string]interface{}
	if handle != nil {
		config, err = handle(file.Name(), Content)
	} else {
		config, err = utils.Convert2JavaProps(file.Name(), Content)
	}
	if err != nil {
		return fmt.Errorf("failed to pull configurations from [%s] file, %s", file.Name(), err)
	}

	err = cmSource.handlePriority(file.Name(), priority)
	if err != nil {
		return fmt.Errorf("failed to handle priority of [%s], %s", file.Name(), err)
	}

	events := cmSource.compareUpdate(config, file.Name())
	if cmSource.watchPool != nil && cmSource.watchPool.callback != nil { // if file source already added and try to add
		for _, e := range events {
			cmSource.watchPool.callback.OnEvent(e)
		}
	}

	return nil
}

func (cmSource *configMapSource) handlePriority(filePath string, priority uint32) error {
	cmSource.Lock()
	newFilePriority := make([]file, 0)
	var prioritySet bool
	for _, f := range cmSource.files {

		if f.filePath == filePath && f.priority == priority {
			prioritySet = true
			newFilePriority = append(newFilePriority, file{
				filePath: filePath,
				priority: priority,
			})
		}
		newFilePriority = append(newFilePriority, f)
	}

	if !prioritySet {
		newFilePriority = append(newFilePriority, file{
			filePath: filePath,
			priority: priority,
		})
	}

	cmSource.files = newFilePriority
	cmSource.Unlock()

	return nil
}

func (cmSource *configMapSource) GetConfigurations() (map[string]interface{}, error) {
	configMap := make(map[string]interface{})

	cmSource.Lock()
	defer cmSource.Unlock()
	for key, confInfo := range cmSource.Configurations {
		if confInfo == nil {
			configMap[key] = nil
			continue
		}

		configMap[key] = confInfo.Value
	}

	return configMap, nil
}

func (cmSource *configMapSource) GetConfigurationByKey(key string) (interface{}, error) {
	cmSource.Lock()
	defer cmSource.Unlock()

	for ckey, confInfo := range cmSource.Configurations {
		if confInfo == nil {
			confInfo.Value = nil
			continue
		}

		if ckey == key {
			return confInfo.Value, nil
		}
	}

	return nil, errors.New("key does not exist")
}

func (*configMapSource) GetSourceName() string {
	return ConfigMapConfigSourceConst
}

func (cmSource *configMapSource) GetPriority() int {
	return cmSource.priority
}

//SetPriority custom priority
func (cmSource *configMapSource) SetPriority(priority int) {
	cmSource.priority = priority
}

func (cmSource *configMapSource) DynamicConfigHandler(callback core.DynamicConfigCallback) error {
	if callback == nil {
		return errors.New("call back can not be nil")
	}

	watchPool, err := newWatchPool(callback, cmSource)
	if err != nil {
		return err
	}

	cmSource.watchPool = watchPool

	go cmSource.watchPool.startWatchPool()

	return nil
}

func newWatchPool(callback core.DynamicConfigCallback, cfgSrc *configMapSource) (*watch, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		openlogging.GetLogger().Error("New file watcher failed:" + err.Error())
		return nil, err
	}

	watch := new(watch)
	watch.callback = callback
	watch.configMapSource = cfgSrc
	watch.watcher = watcher
	openlogging.GetLogger().Info("create new watcher")
	return watch, nil
}

func (wth *watch) startWatchPool() {
	go wth.watchFile()
	for _, file := range wth.configMapSource.files {
		f, err := filepath.Abs(file.filePath)
		if err != nil {
			openlogging.GetLogger().Errorf("failed to get Directory info from: %s file: %s", file.filePath, err)
			return
		}

		err = wth.watcher.Add(f)
		if err != nil {
			openlogging.GetLogger().Errorf("add watcher file: %+v fail %s", file, err)
			return
		}
	}
}

func (wth *watch) AddWatchFile(filePath string) {
	err := wth.watcher.Add(filePath)
	if err != nil {
		openlogging.GetLogger().Errorf("add watcher file: %s fail: %s", filePath, err)
		return
	}
}

func (wth *watch) watchFile() {
	for {
		select {
		case event, ok := <-wth.watcher.Events:
			if !ok {
				openlogging.GetLogger().Warnf("file watcher stop")
				return
			}

			if strings.HasSuffix(event.Name, ".swx") || strings.HasSuffix(event.Name, ".swp") || strings.HasSuffix(event.Name, "~") {
				//ignore
				continue
			}
			//openlogging.GetLogger().Debugf("file event %s, operation is %d. reload it.", event.Name, event.Op)

			if event.Op == fsnotify.Remove {
				//openlogging.GetLogger().Warnf("the file change mode: %s, continue", event.String())
				continue
			}

			if event.Op == fsnotify.Rename {
				wth.watcher.Remove(event.Name)
				// check existence of file
				_, err := os.Open(event.Name)
				if os.IsNotExist(err) {
					openlogging.GetLogger().Warnf("[%s] file does not exist so not able to watch further", event.Name, err)
				} else {
					wth.AddWatchFile(event.Name)
				}

				continue
			}

			if event.Op == fsnotify.Create {
				time.Sleep(time.Millisecond)
			}

			wth.configMapSource.updateFile(wth, event)

		case err := <-wth.watcher.Errors:
			openlogging.GetLogger().Debugf("watch file error:", err)
			return
		}
	}
}

func (cmSource *configMapSource) updateFile(wth *watch, event fsnotify.Event) {
	if wth.configMapSource.isFileSrcExist(event.Name) {
		handle := wth.configMapSource.fileHandlers[event.Name]
		if handle == nil {
			handle = utils.Convert2JavaProps
		}
		content, err := ioutil.ReadFile(event.Name)
		if err != nil {
			openlogging.GetLogger().Error("read file error " + err.Error())
			return
		}

		newConf, err := handle(event.Name, content)
		if err != nil {
			openlogging.GetLogger().Error("convert error " + err.Error())
			return
		}
		events := wth.configMapSource.compareUpdate(newConf, event.Name)
		//openlogging.GetLogger().Debugf("Event generated events %s", events)
		for _, e := range events {
			wth.callback.OnEvent(e)
		}
	} else {
		var priority uint32 = configMapSourcePriority
		for _, file := range wth.configMapSource.files {
			if strings.Contains(event.Name, file.filePath) {
				priority = file.priority
			}
		}

		var fileHandler utils.FileHandler
		for path, handler := range wth.configMapSource.fileHandlers {
			if strings.Contains(event.Name, path) {
				fileHandler = handler
			}
		}
		wth.configMapSource.AddFile(event.Name, priority, fileHandler)
	}

}

func (cmSource *configMapSource) compareUpdate(newconf map[string]interface{}, filePath string) []*core.Event {
	events := make([]*core.Event, 0)
	fileConfs := make(map[string]*ConfigInfo)
	if cmSource == nil {
		return nil
	}

	cmSource.Lock()
	defer cmSource.Unlock()

	var filePathPriority uint32 = math.MaxUint32
	for _, file := range cmSource.files {
		if file.filePath == filePath {
			filePathPriority = file.priority
		}
	}

	if filePathPriority == math.MaxUint32 {
		return nil
	}

	for key, confInfo := range cmSource.Configurations {
		if confInfo == nil {
			confInfo.Value = nil
			continue
		}

		switch confInfo.FilePath {
		case filePath:
			newConfValue, ok := newconf[key]
			if !ok {
				events = append(events, &core.Event{EventSource: ConfigMapConfigSourceConst, Key: key,
					EventType: core.Delete, Value: confInfo.Value})
				continue
			} else if reflect.DeepEqual(confInfo.Value, newConfValue) {
				fileConfs[key] = confInfo
				continue
			}

			confInfo.Value = newConfValue
			fileConfs[key] = confInfo

			events = append(events, &core.Event{EventSource: ConfigMapConfigSourceConst, Key: key,
				EventType: core.Update, Value: newConfValue})

		default:
			newConfValue, ok := newconf[key]
			if ok {
				var priority uint32 = math.MaxUint32
				for _, file := range cmSource.files {
					if file.filePath == confInfo.FilePath {
						priority = file.priority
					}
				}

				if priority == filePathPriority {
					fileConfs[key] = confInfo
					confInfo.Value = newconf[key]
					//openlogging.GetLogger().Infof("Two files have same priority. use new value: %s ", confInfo.FilePath)

				} else if filePathPriority < priority { // lower the vale higher is the priority
					confInfo.Value = newConfValue
					fileConfs[key] = confInfo
					events = append(events, &core.Event{EventSource: ConfigMapConfigSourceConst,
						Key: key, EventType: core.Update, Value: newConfValue})

				} else {
					fileConfs[key] = confInfo
				}
			} else {
				fileConfs[key] = confInfo
			}
		}
	}

	fileConfs, events = cmSource.addOrCreateConf(fileConfs, newconf, events, filePath)

	cmSource.Configurations = fileConfs

	return events
}

func (cmSource *configMapSource) addOrCreateConf(fileConfs map[string]*ConfigInfo, newconf map[string]interface{},
	events []*core.Event, filePath string) (map[string]*ConfigInfo, []*core.Event) {
	for key, value := range newconf {
		handled := false

		_, ok := fileConfs[key]
		if ok {
			handled = true
		}

		if !handled {
			events = append(events, &core.Event{EventSource: ConfigMapConfigSourceConst, Key: key,
				EventType: core.Create, Value: value})
			fileConfs[key] = &ConfigInfo{
				FilePath: filePath,
				Value:    value,
			}
		}
	}

	return fileConfs, events
}

func (cmSource *configMapSource) Cleanup() error {

	cmSource.fileLock.Lock()
	defer cmSource.fileLock.Unlock()

	if configMapConfigSource == nil || cmSource == nil {
		return nil
	}

	if cmSource.watchPool != nil && cmSource.watchPool.watcher != nil {
		cmSource.watchPool.watcher.Close()
	}

	if cmSource.watchPool != nil {
		cmSource.watchPool.configMapSource = nil
		cmSource.watchPool.callback = nil
		cmSource.watchPool = nil
	}
	cmSource.Configurations = nil
	cmSource.files = make([]file, 0)
	return nil
}

func (cmSource *configMapSource) GetConfigurationByKeyAndDimensionInfo(key, di string) (interface{}, error) {
	return nil, nil
}

func (cmSource *configMapSource) AddDimensionInfo(dimensionInfo string) (map[string]string, error) {
	return nil, nil
}

func (cmSource *configMapSource) GetConfigurationsByDI(dimensionInfo string) (map[string]interface{}, error) {
	return nil, nil
}
