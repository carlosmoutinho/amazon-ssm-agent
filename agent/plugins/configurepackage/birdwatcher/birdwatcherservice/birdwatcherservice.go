// Copyright 2017 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package birdwatcherservice

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/fileutil/artifact"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/birdwatcher"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/birdwatcher/archive"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/birdwatcher/birdwatcherarchive"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/birdwatcher/documentarchive"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/birdwatcher/facade"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/envdetect"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/packageservice"
	"github.com/aws/amazon-ssm-agent/agent/plugins/configurepackage/trace"
	"github.com/aws/aws-sdk-go/service/ssm"
)

// NanoTime is helper interface for mocking time
type NanoTime interface {
	NowUnixNano() int64
}

type TimeImpl struct {
}

func (t *TimeImpl) NowUnixNano() int64 {
	return time.Now().UnixNano()
}

// PackageService is the concrete type for Birdwatcher PackageService
type PackageService struct {
	pkgSvcName    string
	facadeClient  facade.BirdwatcherFacade
	manifestCache packageservice.ManifestCache
	collector     envdetect.Collector
	timeProvider  NanoTime
	archive       archive.IPackageArchive
}

func NewBirdwatcherArchive(facadeClient facade.BirdwatcherFacade, manifestCache packageservice.ManifestCache, birdwatcherManifest string) packageservice.PackageService {
	pkgArchive := birdwatcherarchive.New(facadeClient, birdwatcherManifest)
	return New(pkgArchive, facadeClient, manifestCache, packageservice.PackageServiceName_birdwatcher)
}

func NewDocumentArchive(facadeClient facade.BirdwatcherFacade, manifestCache packageservice.ManifestCache) packageservice.PackageService {
	pkgArchive := documentarchive.New(facadeClient)
	return New(pkgArchive, facadeClient, manifestCache, packageservice.PackageServiceName_document)
}

// New constructor for PackageService
func New(pkgArchive archive.IPackageArchive, facadeClient facade.BirdwatcherFacade, manifestCache packageservice.ManifestCache, name string) packageservice.PackageService {

	return &PackageService{
		pkgSvcName:    name,
		facadeClient:  facadeClient,
		manifestCache: manifestCache,
		collector:     &envdetect.CollectorImp{},
		timeProvider:  &TimeImpl{},
		archive:       pkgArchive,
	}
}

func (ds *PackageService) PackageServiceName() string {
	return ds.pkgSvcName
}

func (ds *PackageService) GetPackageArnAndVersion(packageName string, packageVersion string) (name string, version string) {
	return ds.archive.GetResourceVersion(packageName, packageVersion)
}

// DownloadManifest downloads the manifest for a given version (or latest) and returns the agent version specified in manifest
func (ds *PackageService) DownloadManifest(tracer trace.Tracer, packageName string, version string) (string, string, bool, error) {
	manifest, isSameAsCache, err := downloadManifest(ds, packageName, version)
	if err != nil {
		return "", "", isSameAsCache, err
	}
	return ds.archive.GetResourceArn(manifest), manifest.Version, isSameAsCache, nil
}

// DownloadArtifact downloads the platform matching artifact specified in the manifest
func (ds *PackageService) DownloadArtifact(tracer trace.Tracer, packageName string, version string) (string, error) {
	trace := tracer.BeginSection("download artifact")
	manifest, err := readManifestFromCache(ds.manifestCache, packageName, version)
	if err != nil {
		trace.AppendInfof("error when reading the manifest from cache %v", err).End()
		manifest, _, err = downloadManifest(ds, packageName, version)
		if err != nil {
			trace.WithError(err).End()
			return "", fmt.Errorf("failed to download the manifest: %v", err)
		}
	}

	file, err := ds.findFileFromManifest(tracer, manifest)
	if err != nil {
		trace.WithError(err).End()
		return "", err
	}

	trace.End()
	return downloadFile(ds, tracer, file, packageName, version)
}

// ReportResult sents back the result of the install/upgrade/uninstall run back to Birdwatcher
func (ds *PackageService) ReportResult(tracer trace.Tracer, result packageservice.PackageResult) error {
	log := tracer.CurrentTrace().Logger
	env, _ := ds.collector.CollectData(log)

	var previousPackageVersion *string
	if result.PreviousPackageVersion != "" {
		previousPackageVersion = &result.PreviousPackageVersion
	}

	var steps []*ssm.ConfigurePackageResultStep
	for _, t := range result.Trace {
		timing := (t.Timing - result.Timing) / 1000000 // converting nano to miliseconds
		steps = append(steps,
			&ssm.ConfigurePackageResultStep{
				Action: &t.Operation,
				Result: &t.Exitcode,
				Timing: &timing,
			})
	}

	overallTiming := (ds.timeProvider.NowUnixNano() - result.Timing) / 1000000

	input := &ssm.PutConfigurePackageResultInput{
		PackageName:            &result.PackageName,
		PackageVersion:         &result.Version,
		PreviousPackageVersion: previousPackageVersion,
		Operation:              &result.Operation,
		OverallTiming:          &overallTiming,
		Result:                 &result.Exitcode,
		Attributes: map[string]*string{
			"platformName":     &env.OperatingSystem.Platform,
			"platformVersion":  &env.OperatingSystem.PlatformVersion,
			"architecture":     &env.OperatingSystem.Architecture,
			"instanceID":       &env.Ec2Infrastructure.InstanceID,
			"instanceType":     &env.Ec2Infrastructure.InstanceType,
			"region":           &env.Ec2Infrastructure.Region,
			"availabilityZone": &env.Ec2Infrastructure.AvailabilityZone,
		},
		Steps: steps,
	}

	_, err := ds.facadeClient.PutConfigurePackageResult(input)

	if err != nil {
		return fmt.Errorf("failed to report results: %v", err)
	}

	return nil
}

// utils
func readManifestFromCache(cache packageservice.ManifestCache, packageName string, version string) (*birdwatcher.Manifest, error) {
	data, err := cache.ReadManifest(packageName, version)
	if err != nil {
		return nil, err
	}

	return parseManifest(&data)
}

func downloadManifest(ds *PackageService, packageName string, version string) (*birdwatcher.Manifest, bool, error) {
	isSameAsCache := false
	if ds == nil {
		return nil, isSameAsCache, fmt.Errorf("PackageService doesn't exist")
	}
	manifest, err := ds.archive.DownloadArchiveInfo(packageName, version)
	if err != nil {
		return nil, isSameAsCache, fmt.Errorf("failed to download manifest - %v", err)
	}

	byteManifest := []byte(manifest)

	parsedManifest, err := parseManifest(&byteManifest)
	if err != nil {
		return nil, isSameAsCache, err
	}

	cachedManifest, err := readManifestFromCache(ds.manifestCache, ds.archive.GetResourceArn(parsedManifest), parsedManifest.Version)

	if reflect.DeepEqual(parsedManifest, cachedManifest) {
		isSameAsCache = true
	}

	err = ds.manifestCache.WriteManifest(ds.archive.GetResourceArn(parsedManifest), parsedManifest.Version, byteManifest)
	if err != nil {
		return nil, isSameAsCache, fmt.Errorf("failed to write manifest to file: %v", err)
	}

	return parsedManifest, isSameAsCache, nil
}

func parseManifest(data *[]byte) (*birdwatcher.Manifest, error) {
	var manifest birdwatcher.Manifest

	// TODO: additional validation
	if err := json.NewDecoder(bytes.NewReader(*data)).Decode(&manifest); err != nil {
		return nil, fmt.Errorf("failed to decode manifest: %v", err)
	}

	return &manifest, nil
}

func (ds *PackageService) findFileFromManifest(tracer trace.Tracer, manifest *birdwatcher.Manifest) (*archive.File, error) {
	var fileInfo *birdwatcher.FileInfo
	var file archive.File
	var filename string

	pkginfo, err := ds.extractPackageInfo(tracer, manifest)
	if err != nil {
		return nil, fmt.Errorf("failed to find platform: %v", err)
	}

	for name, f := range manifest.Files {
		if name == pkginfo.FileName {
			fileInfo = f
			filename = name
			break
		}
	}

	if fileInfo == nil {
		return nil, fmt.Errorf("failed to find file for %+v", pkginfo)
	}
	file.Info = *fileInfo
	file.Name = filename

	return &file, nil
}

func downloadFile(ds *PackageService, tracer trace.Tracer, file *archive.File, packagename string, version string) (string, error) {
	if ds == nil || ds.archive == nil || file == nil {
		return "", fmt.Errorf("Either package service does not exist or does not have archive information or the file information does not exist")
	}
	sourceUrl, err := ds.archive.GetFileDownloadLocation(file, packagename, version)
	if err != nil {
		return "", err
	}
	downloadInput := artifact.DownloadInput{
		SourceURL: sourceUrl,
		// TODO don't hardcode sha256 - use multiple checksums
		SourceChecksums: file.Info.Checksums,
	}

	log := tracer.CurrentTrace().Logger
	downloadOutput, downloadErr := birdwatcher.Networkdep.Download(log, downloadInput)
	if downloadErr != nil || downloadOutput.LocalFilePath == "" {
		errMessage := fmt.Sprintf("failed to download installation package reliably, %v", downloadInput.SourceURL)
		if downloadErr != nil {
			errMessage = fmt.Sprintf("%v, %v", errMessage, downloadErr.Error())
		}
		// TODO: attempt to clean up failed download folder?

		// return download error
		return "", errors.New(errMessage)
	}

	return downloadOutput.LocalFilePath, nil
}

// ExtractPackageInfo returns the correct PackageInfo for the current instances platform/version/arch
func (ds *PackageService) extractPackageInfo(tracer trace.Tracer, manifest *birdwatcher.Manifest) (*birdwatcher.PackageInfo, error) {
	log := tracer.CurrentTrace().Logger
	env, err := ds.collector.CollectData(log)
	if err != nil {
		return nil, fmt.Errorf("failed to collect data: %v", err)
	}

	if keyplatform, ok := matchPackageSelectorPlatform(env.OperatingSystem.Platform, manifest.Packages); ok {
		if keyversion, ok := matchPackageSelectorVersion(env.OperatingSystem.PlatformVersion, manifest.Packages[keyplatform]); ok {
			if keyarch, ok := matchPackageSelectorArch(env.OperatingSystem.Architecture, manifest.Packages[keyplatform][keyversion]); ok {
				return manifest.Packages[keyplatform][keyversion][keyarch], nil
			}
		}
	}

	return nil, fmt.Errorf("no manifest found for platform: %s, version %s, architecture %s",
		env.OperatingSystem.Platform, env.OperatingSystem.PlatformVersion, env.OperatingSystem.Architecture)
}

func matchPackageSelectorPlatform(key string, dict map[string]map[string]map[string]*birdwatcher.PackageInfo) (string, bool) {
	if _, ok := dict[key]; ok {
		return key, true
	} else if _, ok := dict["_any"]; ok {
		return "_any", true
	}

	return "", false
}

func matchPackageSelectorVersion(key string, dict map[string]map[string]*birdwatcher.PackageInfo) (string, bool) {
	if _, ok := dict[key]; ok {
		return key, true
	} else if _, ok := dict["_any"]; ok {
		return "_any", true
	}

	return "", false
}

func matchPackageSelectorArch(key string, dict map[string]*birdwatcher.PackageInfo) (string, bool) {
	if _, ok := dict[key]; ok {
		return key, true
	} else if _, ok := dict["_any"]; ok {
		return "_any", true
	}

	return "", false
}
