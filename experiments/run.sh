echo "================== RUNNING=================="
date

isOfficial=false

BEST_FIT="bestfit"
OVER_SUB="oversub"
PROPOSED="proposed"
ONE_SHOT="oneshot"
WORST_FIT="worstfit"
GENERIC="generic"

oversub=1.5
nodeNum=5000
cpuPerNode=64
memPerNode=128

totalPodNumber=25000000
workloadSubsetFactor=1
isDebug=true
workloadSubfolderCap=100000
start="2019-01-01T00:00:00+09:00"
end="2019-01-01T05:00:00+09:00"
startTrace="000000000"
targetNum=0

if $isOfficial
then
    nodeNum=5000
    totalPodNumber=25000000
    start="2019-01-01T00:00:00+09:00"
    end="2019-01-01T12:00:00+09:00"
    pathToTrace="/dev/tan/ResourceAllocation/parse/results/tasks-res"
    pathToWorkload="/proj/yarnrm-PG0/google-trace-data/workload"
    log_path="/proj/yarnrm-PG0/google-trace-data"
    tick=60
    metricsTick=60
else
    echo reach here.
	nodeNum=5
    totalPodNumber=100
    targetNum=64
    start="2019-01-01T00:00:00+09:00"
    end="2019-01-01T00:30:00+09:00"
	pathToTrace="/ssd/projects/google-trace-data/task-res"
    pathToWorkload="./tmp/workload"
    log_path="./log"
    tick=1
    metricsTick=1
fi

mkdir $pathToWorkload
mkdir $log_path

runSim(){
    ./gen_config.sh $1 "." $nodeNum $cpuPerNode $memPerNode $tick $metricsTick "$start" $log_path
    go run $(go list ./...) --config="./config/cluster_$1" \
    --workload="$pathToWorkload"  \
    --scheduler="$1" \
    --isgen=$2 \
    --oversub=$oversub \
    --istrace=$3 \
    --trace="$pathToTrace" \
    --start="$start" \
    --end="$end" \
    --trace-start="$startTrace" \
    --tick="$tick" \
    --total-pods-num=$totalPodNumber \
    --target-pod-num=$targetNum \
    --subset-factor=$workloadSubsetFactor \
    --workload-subfolder-cap=$workloadSubfolderCap \
    &> run_${1}.out
}
#rm -rf *.out
SECONDS=0
runSim $GENERIC true false
echo "Generating workload took $SECONDS seconds"


if $isOfficial
then
    SECONDS=0 
    echo "running simulation"
    runSim $WORST_FIT false false &
    runSim $OVER_SUB false false &
    runSim $ONE_SHOT false false &
    wait
    echo "simulation took $SECONDS seconds"
else
    echo "running simulation"

    SECONDS=0 
	runSim $WORST_FIT false false
    echo "$WORST_FIT took $SECONDS seconds"

    SECONDS=0 
    runSim $OVER_SUB false false
    echo "$OVER_SUB took $SECONDS seconds"

    SECONDS=0 
    runSim $ONE_SHOT false false    
    echo "$ONE_SHOT took $SECONDS seconds"
fi


SECONDS=0 
echo "==================Plotting=================="
python3 plotResults.py $log_path
echo "plotResults.py took $SECONDS seconds"
echo "==================FINISHED=================="
date