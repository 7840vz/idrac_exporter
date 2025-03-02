package redfish

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"github.com/mrlhansen/idrac_exporter/internal/config"
	"github.com/mrlhansen/idrac_exporter/internal/logging"
)

const redfishRootPath = "/redfish/v1"
var logger = logging.NewLogger().Sugar()

type systemMetricsStore interface {
	SetPowerOn(on bool)
	SetHealthOk(ok bool, status string)
	SetLedOn(on bool, state string)
	SetMemorySize(memory float64)
	SetCpuCount(numCpus int, model string)
	SetBiosVersion(version string)
	SetMachineInfo(manufacturer, model, serial, sku string)
}

type sensorsMetricsStore interface {
	SetTemperature(temperature float64, name, units string)
	SetFanSpeed(speed float64, name, units string)
}

type powerMetricsStore interface {
	SetPowerSupplyInputWatts(value float64, id string)
	SetPowerSupplyInputVoltage(value float64, id string)
	SetPowerSupplyOutputWatts(value float64, id string)
	SetPowerSupplyCapacityWatts(value float64, id string)
	SetPowerSupplyEfficiencyPercent(value float64, id string)

	SetPowerControlConsumedWatts(value float64, id, name string)
	SetPowerControlMinConsumedWatts(value float64, id, name string)
	SetPowerControlMaxConsumedWatts(value float64, id, name string)
	SetPowerControlAvgConsumedWatts(value float64, id, name string)
	SetPowerControlCapacityWatts(value float64, id, name string)
	SetPowerControlInterval(interval int, id, name string)
}

type selStore interface {
	AddSelEntry(id string, message string, component string, severity string, created time.Time)
}

type Client struct {
	hostname    string
	basicAuth   string
	httpClient  *http.Client
	systemPath  string
	thermalPath string
	powerPath   string
}

func newHttpClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		Timeout: time.Duration(config.Config.Timeout) * time.Second,
	}
}

func NewClient(hostConfig *config.HostConfig) (*Client, error) {
	client := &Client{
		hostname:   hostConfig.Hostname,
		basicAuth:  hostConfig.Token,
		httpClient: newHttpClient(),
	}

	err := client.findAllEndpoints();
	if err != nil {
		return nil, err
	}

	return client, nil
}

func (client *Client) findAllEndpoints() error {
	var root V1Response
	var group GroupResponse
	var chassis ChassisResponse
	var err error

	// Root
	err = client.redfishGet(redfishRootPath, &root);
	if err != nil {
		return err
	}

	// System
	err = client.redfishGet(root.Systems.OdataId, &group);
	if err != nil {
		return err
	}

	client.systemPath = group.Members[0].OdataId

	// Chassis
	err = client.redfishGet(root.Chassis.OdataId, &group);
	if err != nil {
		return err
	}

	// Thermal and Power
	err = client.redfishGet(group.Members[0].OdataId, &chassis);
	if err != nil {
		return err
	}

	client.thermalPath = chassis.Thermal.OdataId
	client.powerPath = chassis.Power.OdataId

	return nil
}

func (client *Client) RefreshSensors(store sensorsMetricsStore) error {
	var resp ThermalResponse

	err := client.redfishGet(client.thermalPath, &resp);
	if err != nil {
		return err
	}

	for _, t := range resp.Temperatures {
		if t.Status.State != StateEnabled {
			continue
		}
		store.SetTemperature(t.ReadingCelsius, t.Name, "celsius")
	}

	for _, f := range resp.Fans {
		if f.Status.State != StateEnabled {
			continue
		}

		name := f.GetName()
		if name == "" {
			continue
		}

		units := f.GetUnits()
		if units == "" {
			continue
		}

		store.SetFanSpeed(f.GetReading(), name, strings.ToLower(units))
	}

	return nil
}

func (client *Client) RefreshSystem(store systemMetricsStore) error {
	var resp SystemResponse

	err := client.redfishGet(client.systemPath, &resp);
	if err != nil {
		return err
	}

	store.SetPowerOn(resp.PowerState == "On")
	store.SetHealthOk(resp.Status.Health == "OK", resp.Status.Health)
	store.SetLedOn(resp.IndicatorLED != "Off", resp.IndicatorLED)

	value := resp.MemorySummary.TotalSystemMemoryGiB // depending on the bios version, this is reported in either GB or GiB
	if value == math.Trunc(value) {
		value = value * 1024
	} else {
		value = math.Floor(value * 1099511627776.0 / 1e9)
	}
	store.SetMemorySize(value)

	store.SetCpuCount(resp.ProcessorSummary.Count, resp.ProcessorSummary.Model)
	store.SetBiosVersion(resp.BiosVersion)
	store.SetMachineInfo(resp.Manufacturer, resp.Model, resp.SerialNumber, resp.SKU)

	return nil
}

func (client *Client) RefreshPower(store powerMetricsStore) error {
	var resp PowerResponse

	err := client.redfishGet(client.powerPath, &resp);
	if err != nil {
		return err
	}

	for i, psu := range resp.PowerSupplies {
		if psu.Status.State != StateEnabled {
			continue
		}

		id := strconv.Itoa(i)
		store.SetPowerSupplyInputWatts(psu.PowerInputWatts, id)
		store.SetPowerSupplyInputVoltage(psu.LineInputVoltage, id)
		store.SetPowerSupplyOutputWatts(psu.GetOutputPower(), id)
		store.SetPowerSupplyCapacityWatts(psu.PowerCapacityWatts, id)
		store.SetPowerSupplyEfficiencyPercent(psu.EfficiencyPercent, id)
	}

	for i, pc := range resp.PowerControl {
		id := strconv.Itoa(i)
		store.SetPowerControlConsumedWatts(pc.PowerConsumedWatts, id, pc.Name)
		store.SetPowerControlCapacityWatts(pc.PowerCapacityWatts, id, pc.Name)

		if pc.PowerMetrics == nil {
			continue
		}

		pm := pc.PowerMetrics
		store.SetPowerControlMinConsumedWatts(pm.MinConsumedWatts, id, pc.Name)
		store.SetPowerControlMaxConsumedWatts(pm.MaxConsumedWatts, id, pc.Name)
		store.SetPowerControlAvgConsumedWatts(pm.AverageConsumedWatts, id, pc.Name)
		store.SetPowerControlInterval(pm.IntervalInMin, id, pc.Name)
	}

	return nil
}

func (client *Client) RefreshIdracSel(store selStore) error {
	var resp IdracSelResponse

	err := client.redfishGet(redfishRootPath + "/Managers/iDRAC.Embedded.1/Logs/Sel", &resp);
	if err != nil {
		return err
	}

	for _, e := range resp.Members {
		store.AddSelEntry(e.Id, e.Message, e.SensorType, e.Severity, e.Created)
	}

	return nil
}

func (client *Client) redfishGet(path string, res interface{}) error {
	url := "https://" + client.hostname + path

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}

	req.Header.Add("Authorization", "Basic " + client.basicAuth)
	req.Header.Add("Accept", "application/json")

	logger.Debugf("Querying url %q", url)

	resp, err := client.httpClient.Do(req)
	if err != nil {
		logger.Debugf("Failed to query url %q: %v", url, err)
		return err
	}

	if resp.StatusCode != 200 {
		logger.Debugf("Query to url %q returned unexpected status code: %d (%s)", url, resp.StatusCode, resp.Status)
		return fmt.Errorf("%d %s", resp.StatusCode, resp.Status)
	}

	err = json.NewDecoder(resp.Body).Decode(res);
	if err != nil {
		logger.Debugf("Error decoding response from url %q: %v", url, err)
		return err
	}

	return nil
}
