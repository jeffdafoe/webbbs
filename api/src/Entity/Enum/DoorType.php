<?php

namespace App\Entity\Enum;

enum DoorType: string
{
    case INTERACTIVE = 'interactive';  // Real-time, can interrupt users (ZChat)
    case TURN_BASED = 'turn_based';    // User enters, takes turns, exits (TradeWars)
    case BACKGROUND = 'background';     // Runs in background, notifications only
}
