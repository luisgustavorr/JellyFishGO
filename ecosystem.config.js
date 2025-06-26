exports.apps = [
    {
        name: "jellyFishGO",
        script: "jellyFish",
        interpreter: "none", // instruct PM2 that the "script" doesn't rely on >
        max_memory_restart: '700M'
    }
];
