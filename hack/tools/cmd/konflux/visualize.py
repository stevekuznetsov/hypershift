#!/usr/bin/env python3

import json
import argparse
import os
from datetime import datetime, timedelta
from datetime import timezone

import pandas as pd
import matplotlib.pyplot as plt
import matplotlib.ticker as tick

color_blue='#2171b5'
color_light_blue='#6baed6'
color_green='#74c476'
color_red='#fb6a4a'

parser = argparse.ArgumentParser()
parser.add_argument("-d", "--data-dir", help="Directory for input and output.", required=True)
parser.add_argument("-f", "--data-file", help="File name containing commit summaries, relative to the directory.", default="summary.json")
args = parser.parse_args()

with open(os.path.join(args.data_dir, args.data_file)) as f:
    rawData = json.load(f)

rawCommits = [{
    'date': datetime.fromisoformat(i['date']).astimezone(timezone.utc),
    'built': 1,
    'published': int(i['published'])
} for i in rawData]
commits = pd.DataFrame(
    data=[{'built': i['built'], 'published': i['published']} for i in rawCommits],
    index=pd.DatetimeIndex([i['date'] for i in rawCommits])
)
commits = commits.resample('W').sum()

fig = plt.figure()
fig.suptitle('Konflux Build Performance for HyperShift')
countplot = fig.add_subplot(311)
countplot.stackplot(
    commits.index, commits.built - commits.published, commits.published,
    step='mid', colors=[color_blue, color_light_blue]
)
countplot.legend(labels=['Built', 'Published'], loc='upper left')
countplot.set_ylim(0, commits.built.max() * 1.1)
countplot.set_xlim(commits.index[0], commits.index[-1])
countplot.tick_params(axis='x', which='both', bottom=False, top=False, labelbottom=False)
countplot.set_ylabel('Commits')

fractionplot = fig.add_subplot(312, sharex=countplot)
f = commits.published.astype(float) / commits.built.astype(float)
fractionplot.stackplot(commits.index, f * 100, (1 - f) * 100, step='mid', colors=[color_green, color_red])
fractionplot.set_ylim(0, 100)
countplot.tick_params(axis='x', which='both', bottom=False, top=False, labelbottom=False)
fractionplot.yaxis.set_major_formatter(tick.PercentFormatter())
fractionplot.set_ylabel('Publication Rate')

def roundedTime(time):
    return (time - timedelta(days=time.day)).replace(hour=0, minute=0, second=0, microsecond=0)

rawTags = [{
    'date': roundedTime(datetime.fromisoformat(i['date']).astimezone(timezone.utc)),
    'commitDate': datetime.fromisoformat(i['date']).astimezone(timezone.utc),
    'publishDate': datetime.fromisoformat(i['publishedTime']).astimezone(timezone.utc)
} for i in rawData if i['published']]
tags = pd.DataFrame(
    data=[{'date': i['date'], 'duration': i['publishDate'] - i['commitDate']} for i in rawTags]
)

durationplot = fig.add_subplot(313)
intervals = []
values = []
for interval, durations in tags.groupby(['date'])['duration']:
    intervals.append(interval[0])
    values.append(durations.sort_values().transform(lambda x: x.total_seconds()).values)

durationplot.boxplot(values)

def format_duration(seconds, x):
    if seconds < 60:
        return '{:2.0f}s'.format(seconds)
    if seconds < 3600:
        minutes, seconds = divmod(seconds, 60)
        return '{:2.0f}m{:2.0f}s'.format(minutes, seconds)
    else:
        minutes, seconds = divmod(seconds, 60)
        hours, minutes = divmod(minutes, 60)
        return '{:2.0f}h{:2.0f}m'.format(hours, minutes)


def format_interval(x, pos):
    return intervals[pos].strftime('%Y-%m')

durationplot.set_ylim(0, 60 * 60 * 2.5)
durationplot.yaxis.set_major_formatter(tick.FuncFormatter(format_duration))
durationplot.xaxis.set_major_formatter(tick.FuncFormatter(format_interval))
durationplot.set_ylabel('Image Build Time')

plt.show()